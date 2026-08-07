package data

import (
	"context"
	"sort"
	"time"
)

const retentionDayMS int64 = 86_400_000

type RetentionReport struct {
	GeneratedAt int64 `json:"generatedAt"`
	Active      struct {
		DAU        int `json:"dau"`
		WAU        int `json:"wau"`
		MAU        int `json:"mau"`
		Stickiness int `json:"stickiness"`
	} `json:"active"`
	NewVsReturning struct {
		Window    string `json:"window"`
		NewUsers  int    `json:"newUsers"`
		Returning int    `json:"returning"`
	} `json:"newVsReturning"`
	Cohorts []RetentionCohort `json:"cohorts"`
	PerUser struct {
		Sessions RetentionPercentiles `json:"sessions"`
		Turns    RetentionPercentiles `json:"turns"`
	} `json:"perUser"`
	Totals struct {
		Users    int `json:"users"`
		Sessions int `json:"sessions"`
	} `json:"totals"`
	Attribution string `json:"attribution"`
	Note        string `json:"note"`
}

type RetentionCohort struct {
	Week     string `json:"week"`
	Size     int    `json:"size"`
	Retained []int  `json:"retained"`
}

type RetentionPercentiles struct {
	P50 int `json:"p50"`
	P95 int `json:"p95"`
}

type RetentionRepository struct{ pg *Postgres }

func NewRetentionRepository(pg *Postgres) *RetentionRepository { return &RetentionRepository{pg: pg} }

func (r *RetentionRepository) Report(ctx context.Context, now int64) (RetentionReport, error) {
	report := RetentionReport{GeneratedAt: now, Cohorts: []RetentionCohort{}, Attribution: "DM/personal exact; channel approximate (entries carry no author principal).", Note: "Derived per-request over all sessions × entries; move to a materialized daily rollup if volume grows."}
	report.NewVsReturning.Window = "30d"
	var sessionCount int
	if err := r.pg.Pool.QueryRow(ctx, "SELECT COUNT(*)::int FROM sessions").Scan(&sessionCount); err != nil {
		return report, err
	}
	report.Totals.Sessions = sessionCount
	rows, err := r.pg.Pool.Query(ctx, `SELECT p.principal_id,e.session_id,(e.created_at / 86400000)::bigint,COUNT(*)::int
FROM participants p JOIN sessions s ON s.id=p.session_id JOIN session_entries e ON e.session_id=p.session_id
WHERE e.type='user' AND (e.payload IS NULL OR e.payload NOT LIKE '%"overheard":true%')
  AND (((p.valid_from_seq IS NOT NULL AND e.seq >= p.valid_from_seq) OR (p.valid_from_seq IS NULL AND e.created_at >= p.valid_from))
       AND ((p.valid_to_seq IS NOT NULL AND e.seq < p.valid_to_seq) OR (p.valid_to_seq IS NULL AND (p.valid_to IS NULL OR e.created_at < p.valid_to))))
GROUP BY p.principal_id,e.session_id,(e.created_at / 86400000)::bigint`)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	activeDays := map[string]map[int64]bool{}
	turnsByUser := map[string]int{}
	for rows.Next() {
		var principal string
		var session string
		var day int64
		var turns int
		if err := rows.Scan(&principal, &session, &day, &turns); err != nil {
			return report, err
		}
		_ = session
		if activeDays[principal] == nil {
			activeDays[principal] = map[int64]bool{}
		}
		activeDays[principal][day] = true
		turnsByUser[principal] += turns
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	participantRows, err := r.pg.Pool.Query(ctx, "SELECT p.principal_id,p.session_id FROM participants p JOIN sessions s ON s.id=p.session_id")
	if err != nil {
		return report, err
	}
	defer participantRows.Close()
	sessionsByUser := map[string]map[string]bool{}
	for participantRows.Next() {
		var principal, session string
		if err := participantRows.Scan(&principal, &session); err != nil {
			return report, err
		}
		if sessionsByUser[principal] == nil {
			sessionsByUser[principal] = map[string]bool{}
		}
		sessionsByUser[principal][session] = true
	}
	if err := participantRows.Err(); err != nil {
		return report, err
	}
	today := now / retentionDayMS
	users := make([]string, 0, len(activeDays))
	for user := range activeDays {
		users = append(users, user)
	}
	activeInLast := func(user string, days int64) bool {
		for day := range activeDays[user] {
			if day > today-days && day <= today {
				return true
			}
		}
		return false
	}
	for _, user := range users {
		if activeInLast(user, 1) {
			report.Active.DAU++
		}
		if activeInLast(user, 7) {
			report.Active.WAU++
		}
		if activeInLast(user, 30) {
			report.Active.MAU++
		}
	}
	if report.Active.MAU > 0 {
		report.Active.Stickiness = (report.Active.DAU*100 + report.Active.MAU/2) / report.Active.MAU
	}
	firstDay := map[string]int64{}
	weeksByUser := map[string]map[int64]bool{}
	cohortMembers := map[int64][]string{}
	for _, user := range users {
		minimum := int64(1 << 62)
		weeksByUser[user] = map[int64]bool{}
		for day := range activeDays[user] {
			if day < minimum {
				minimum = day
			}
			weeksByUser[user][day/7] = true
		}
		firstDay[user] = minimum
		cohortMembers[minimum/7] = append(cohortMembers[minimum/7], user)
		if activeInLast(user, 30) {
			if minimum > today-30 {
				report.NewVsReturning.NewUsers++
			} else {
				report.NewVsReturning.Returning++
			}
		}
	}
	weeks := make([]int64, 0, len(cohortMembers))
	for week := range cohortMembers {
		weeks = append(weeks, week)
	}
	sort.Slice(weeks, func(i, j int) bool { return weeks[i] < weeks[j] })
	if len(weeks) > 8 {
		weeks = weeks[len(weeks)-8:]
	}
	for _, week := range weeks {
		members := cohortMembers[week]
		cohort := RetentionCohort{Week: time.UnixMilli(week * 7 * retentionDayMS).UTC().Format("2006-01-02"), Size: len(members), Retained: make([]int, 5)}
		for offset := int64(0); offset < 5; offset++ {
			active := 0
			for _, user := range members {
				if weeksByUser[user][week+offset] {
					active++
				}
			}
			if len(members) > 0 {
				cohort.Retained[offset] = (active*100 + len(members)/2) / len(members)
			}
		}
		report.Cohorts = append(report.Cohorts, cohort)
	}
	sessionCounts, turnCounts := make([]int, 0, len(users)), make([]int, 0, len(users))
	for _, user := range users {
		sessionCounts = append(sessionCounts, len(sessionsByUser[user]))
		turnCounts = append(turnCounts, turnsByUser[user])
	}
	sort.Ints(sessionCounts)
	sort.Ints(turnCounts)
	report.PerUser.Sessions = retentionPercentiles(sessionCounts)
	report.PerUser.Turns = retentionPercentiles(turnCounts)
	report.Totals.Users = len(users)
	return report, nil
}

func retentionPercentiles(values []int) RetentionPercentiles {
	if len(values) == 0 {
		return RetentionPercentiles{}
	}
	index := func(percent int) int { return min(len(values)-1, max(0, (percent*len(values)+99)/100-1)) }
	return RetentionPercentiles{P50: values[index(50)], P95: values[index(95)]}
}
