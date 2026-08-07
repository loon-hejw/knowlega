package data

import (
	"context"
	"strconv"
	"strings"
)

type MetricsRepository struct{ pg *Postgres }

// TurnMetric is the durable counterpart to the Node control-plane metric sample.
// Pointer fields intentionally preserve the distinction between an unmeasured phase
// and a measured zero-duration phase.
type TurnMetric struct {
	TS                                                                                                                                                                                                                                         int64
	ScopeLabel, Status                                                                                                                                                                                                                         string
	SessionID                                                                                                                                                                                                                                  *string
	TurnSeq                                                                                                                                                                                                                                    *int
	RunID                                                                                                                                                                                                                                      *string
	TotalMS                                                                                                                                                                                                                                    int
	TTFTMS, IntakePreambleMS, DispatchMS, ModelCalls, ToolCalls, ProvisionMS, MaterializeMS, CredsMS, LayersMS, CompileMS, RecallMS, ExecMS, StreamMS, LeaseMS, CaptureMS, IngressMS, DetectMS, CompactMS, QueueMS, DeliverMS, SlackInflightMS *int
	Provisioned, ColdStart                                                                                                                                                                                                                     *bool
	CacheRead, CacheWrite, UncachedInput                                                                                                                                                                                                       *int64
}

func NewMetricsRepository(pg *Postgres) *MetricsRepository { return &MetricsRepository{pg: pg} }

func (r *MetricsRepository) PatchByRunID(ctx context.Context, runID string, deliverMS, slackInflightMS *float64) error {
	values := []any{}
	sets := []string{}
	if deliverMS != nil {
		values = append(values, *deliverMS)
		sets = append(sets, "deliver_ms=$"+strconv.Itoa(len(values)))
	}
	if slackInflightMS != nil {
		values = append(values, *slackInflightMS)
		sets = append(sets, "slack_inflight_ms=$"+strconv.Itoa(len(values)))
	}
	if len(sets) == 0 {
		return nil
	}
	values = append(values, runID)
	_, err := r.pg.Pool.Exec(ctx, "UPDATE turn_metrics SET "+strings.Join(sets, ", ")+" WHERE run_id=$"+strconv.Itoa(len(values)), values...)
	return err
}

func (r *MetricsRepository) List(ctx context.Context, scope string, limit int) ([]TurnMetric, error) {
	if limit <= 0 {
		limit = 10000
	}
	args := []any{limit}
	where := ""
	if scope != "" {
		where = " WHERE scope_label=$2"
		args = append(args, scope)
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT ts,scope_label,status,session_id,turn_seq,run_id,total_ms,ttft_ms,intake_preamble_ms,dispatch_ms,provisioned,cold_start,model_calls,tool_calls,provision_ms,materialize_ms,creds_ms,layers_ms,compile_ms,recall_ms,exec_ms,stream_ms,lease_ms,capture_ms,ingress_ms,detect_ms,compact_ms,queue_ms,deliver_ms,slack_inflight_ms,cache_read,cache_write,uncached_input FROM turn_metrics`+where+` ORDER BY ts DESC LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TurnMetric{}
	for rows.Next() {
		var s TurnMetric
		if err := rows.Scan(&s.TS, &s.ScopeLabel, &s.Status, &s.SessionID, &s.TurnSeq, &s.RunID, &s.TotalMS, &s.TTFTMS, &s.IntakePreambleMS, &s.DispatchMS, &s.Provisioned, &s.ColdStart, &s.ModelCalls, &s.ToolCalls, &s.ProvisionMS, &s.MaterializeMS, &s.CredsMS, &s.LayersMS, &s.CompileMS, &s.RecallMS, &s.ExecMS, &s.StreamMS, &s.LeaseMS, &s.CaptureMS, &s.IngressMS, &s.DetectMS, &s.CompactMS, &s.QueueMS, &s.DeliverMS, &s.SlackInflightMS, &s.CacheRead, &s.CacheWrite, &s.UncachedInput); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}
