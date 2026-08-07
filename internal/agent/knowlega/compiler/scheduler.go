package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type sourceTask struct {
	index            int
	attempt          int
	force            bool
	syncOnly         bool
	syncResult       ValidateResult
	updateOnly       bool
	fingerprint      string
	conflictAttempts int
	conflictPaths    []string
}

type sourceTaskResult struct {
	task   sourceTask
	result ValidateResult
	err    error
}

type impactJob struct {
	id                string
	input             ImpactInput
	candidates        map[string]int
	transientAttempts int
}

type impactJobResult struct {
	job      impactJob
	decision ImpactDecision
	err      error
}

type bootstrapTaskState struct {
	Version         int                            `json:"version"`
	Sources         map[string]bootstrapSourceTask `json:"sources"`
	PendingImpacts  map[string][]string            `json:"pending_impacts,omitempty"`
	ImpactDecisions map[string]ImpactDecision      `json:"impact_decisions,omitempty"`
}

type bootstrapSourceTask struct {
	Status           string   `json:"status"`
	Attempts         int      `json:"attempts"`
	ImpactAttempts   int      `json:"impact_attempts,omitempty"`
	ConflictAttempts int      `json:"conflict_attempts,omitempty"`
	ConflictPaths    []string `json:"conflict_paths,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	InputFingerprint string   `json:"input_fingerprint,omitempty"`
	UpdateOnly       bool     `json:"update_only,omitempty"`
}

type SourceTaskExhaustedError struct {
	SourcePath string
	Attempts   int
	Err        error
}

func (e SourceTaskExhaustedError) Error() string {
	return fmt.Sprintf("source task failed after %d attempts: %s: %v", e.Attempts, e.SourcePath, e.Err)
}

func (e SourceTaskExhaustedError) Unwrap() error { return e.Err }

func IsSourceTaskExhaustedError(err error) bool {
	var target SourceTaskExhaustedError
	return errors.As(err, &target)
}

type bootstrapCommitIntent struct {
	SourcePath string   `json:"source_path"`
	Paths      []string `json:"paths"`
}

func validateLLMWikiSourcesConcurrent(opts ValidateOptions, sources []string) (BatchValidateResult, error) {
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	maxAttempts := opts.MaxTaskAttempts
	if maxAttempts <= 0 {
		maxAttempts = 4
	}
	maxConflictAttempts := opts.MaxConflictAttempts
	if maxConflictAttempts <= 0 {
		maxConflictAttempts = 8
	}
	maxImpactAttempts := opts.MaxImpactAttempts
	if maxImpactAttempts <= 0 {
		maxImpactAttempts = 2
	}
	attemptLimit := func(updateOnly bool) int {
		if updateOnly {
			return maxAttempts + maxImpactAttempts
		}
		return maxAttempts
	}
	llmConcurrency := normalizedLLMConcurrency(concurrency, opts.LLMConcurrency)
	gate := newLLMGate(llmConcurrency)
	rawProvider := opts.Provider
	if rawProvider == nil {
		rawProvider = MockProvider{}
	}
	opts.Provider = gatedProvider{provider: rawProvider, gate: gate, observe: opts.OnLLMCall}
	assessor := opts.ImpactAssessor
	if assessor == nil {
		assessor, _ = rawProvider.(ImpactAssessor)
	}
	if assessor != nil {
		assessor = gatedImpactAssessor{assessor: assessor, gate: gate, observe: opts.OnLLMCall}
	}
	state, err := loadBootstrapTaskState(opts.ProjectPath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	forceOnRecovery := map[string]bool{}
	for _, paths := range state.PendingImpacts {
		for _, path := range paths {
			forceOnRecovery[sourceManifestKey(path)] = true
		}
	}
	intents, err := loadBootstrapCommitIntents(opts.ProjectPath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	manifest, err := loadSourceManifest(opts.ProjectPath)
	if err != nil {
		return BatchValidateResult{}, err
	}
	contract, err := GenerationContractSHA256(opts.ProjectPath, opts.MaxFilesPerTask, opts.MaxNewPagesPerSource)
	if err != nil {
		return BatchValidateResult{}, fmt.Errorf("generation contract: %w", err)
	}
	for _, intent := range intents {
		forceOnRecovery[sourceManifestKey(intent.SourcePath)] = true
		changed := map[string]bool{}
		for _, path := range intent.Paths {
			changed[path] = true
		}
		for source, entry := range manifest.Sources {
			for _, path := range entry.Files {
				if changed[path] {
					forceOnRecovery[sourceManifestKey(source)] = true
					break
				}
			}
		}
	}
	state.PendingImpacts = map[string][]string{}
	if state.ImpactDecisions == nil {
		state.ImpactDecisions = map[string]ImpactDecision{}
	}
	allowedSources := make(map[string]bool, len(sources))
	for _, source := range sources {
		allowedSources[source] = true
	}
	for source := range state.Sources {
		if !allowedSources[source] {
			delete(state.Sources, source)
		}
	}
	queue := make([]sourceTask, 0, len(sources))
	restoredConflictQueue := make([]sourceTask, 0)
	for index, source := range sources {
		previous := state.Sources[source]
		hash, hashErr := sourcePathSHA256(source)
		if hashErr != nil {
			return BatchValidateResult{}, hashErr
		}
		fingerprint := sourceTaskInputFingerprint(hash, contract, maxAttempts, maxConflictAttempts, maxImpactAttempts)
		if previous.InputFingerprint != "" && previous.InputFingerprint != fingerprint {
			previous = bootstrapSourceTask{}
			state.Sources[source] = previous
		}
		if previous.InputFingerprint == "" {
			previous.InputFingerprint = fingerprint
		}
		conflictRecovery := previous.Status == "conflicted" || (previous.Status == "processing" && previous.ConflictAttempts > 0 && len(previous.ConflictPaths) > 0)
		transientRecovery := previous.Status == "transient" || ((previous.Status == "failed" || previous.Status == "invalidated") && isPersistedTransientSourceError(previous.Reason))
		if previous.Status != "settled" && previous.Status != "processing" && !conflictRecovery && !transientRecovery && previous.Attempts >= attemptLimit(previous.UpdateOnly) {
			reason := previous.Reason
			if reason == "" {
				reason = "source task attempts exhausted without a recorded error"
			}
			return BatchValidateResult{}, SourceTaskExhaustedError{SourcePath: source, Attempts: previous.Attempts, Err: errors.New(reason)}
		}
		force := forceOnRecovery[sourceManifestKey(source)] || previous.Status == "processing" || previous.Status == "invalidated" || previous.Status == "conflicted" || previous.Status == "impact_pending" || transientRecovery
		attempt := 1
		if previous.Attempts > 0 {
			attempt = previous.Attempts
			// A process restart can leave a task in processing. That attempt never
			// produced a success or failure result, so resume the same attempt
			// number instead of consuming the source's failure budget.
			if !conflictRecovery && !transientRecovery && previous.Status != "processing" && (force || previous.Status != "settled") {
				attempt++
			}
		}
		task := sourceTask{index: index, attempt: attempt, force: force, updateOnly: previous.UpdateOnly, fingerprint: fingerprint, conflictAttempts: previous.ConflictAttempts, conflictPaths: append([]string(nil), previous.ConflictPaths...)}
		if conflictRecovery {
			// A restart may find a conflict task that was marked processing when
			// the process stopped. Until it is actually dispatched again it is a
			// queued conflict, not an active task. Persist that distinction so a
			// second restart and operational audits see truthful state.
			previous.Status = "conflicted"
			state.Sources[source] = previous
			restoredConflictQueue = append(restoredConflictQueue, task)
		} else {
			queue = append(queue, task)
		}
	}
	if err := saveBootstrapTaskState(opts.ProjectPath, state); err != nil {
		return BatchValidateResult{}, err
	}

	jobs := make(chan sourceTask)
	results := make(chan sourceTaskResult, concurrency)
	for worker := 0; worker < concurrency; worker++ {
		go func() {
			for task := range jobs {
				if task.syncOnly {
					var syncErr error
					if opts.OnCommitted != nil {
						syncErr = opts.OnCommitted(task.syncResult)
					}
					results <- sourceTaskResult{task: task, result: task.syncResult, err: syncErr}
					continue
				}
				source := sources[task.index]
				result, taskErr := ValidateLLMWiki(ValidateOptions{
					ProjectPath: opts.ProjectPath, SourcePath: source, Title: opts.Title, Provider: opts.Provider,
					SkipUnchanged: opts.SkipUnchanged && !task.force, OnProgress: opts.OnProgress,
					OnCommitted: opts.OnCommitted, SourceIndex: task.index + 1, SourceTotal: len(sources),
					TaskAttempt: task.attempt, ManagedTask: true,
					ProjectLockHeld:      opts.ProjectLockHeld,
					MaxFilesPerTask:      opts.MaxFilesPerTask,
					MaxNewPagesPerSource: opts.MaxNewPagesPerSource,
					UpdateOnly:           task.updateOnly,
				})
				results <- sourceTaskResult{task: task, result: result, err: taskErr}
			}
		}()
	}
	impactJobs := make(chan impactJob, len(sources))
	impactResults := make(chan impactJobResult, len(sources))
	if assessor != nil {
		go func() {
			for job := range impactJobs {
				if job.transientAttempts > 0 {
					delay := time.Duration(job.transientAttempts) * 500 * time.Millisecond
					if delay > 5*time.Second {
						delay = 5 * time.Second
					}
					time.Sleep(delay)
				}
				decision, assessErr := assessor.AssessImpact(job.input)
				impactResults <- impactJobResult{job: job, decision: decision, err: assessErr}
			}
		}()
	}

	active := 0
	conflictQueue := restoredConflictQueue
	activeConflictPaths := map[int][]string{}
	impactActive := 0
	settled := map[int]ValidateResult{}
	var fatalErr error
	disjoint := func(paths []string) bool {
		used := map[string]bool{}
		for _, activePaths := range activeConflictPaths {
			for _, path := range activePaths {
				used[path] = true
			}
		}
		for _, path := range paths {
			if used[path] {
				return false
			}
		}
		return true
	}
	dispatch := func() error {
		for fatalErr == nil && active < concurrency && len(queue) > 0 {
			task := queue[0]
			queue = queue[1:]
			source := sources[task.index]
			state.Sources[source] = updateBootstrapSourceState(state.Sources[source], "processing", task.attempt, "", task.fingerprint, task.updateOnly)
			if err := saveBootstrapTaskState(opts.ProjectPath, state); err != nil {
				return err
			}
			active++
			jobs <- task
		}
		// Conflict retries are drained only after the optimistic first/failure
		// queue is empty. Retries with disjoint stale paths may still run in
		// parallel; hot-page retries are isolated without holding LLM-time locks.
		for fatalErr == nil && len(queue) == 0 && active-len(activeConflictPaths) == 0 && active < concurrency && len(conflictQueue) > 0 {
			picked := -1
			for i := range conflictQueue {
				if disjoint(conflictQueue[i].conflictPaths) {
					picked = i
					break
				}
			}
			if picked < 0 {
				break
			}
			task := conflictQueue[picked]
			conflictQueue = append(conflictQueue[:picked], conflictQueue[picked+1:]...)
			source := sources[task.index]
			state.Sources[source] = updateBootstrapSourceState(state.Sources[source], "processing", task.attempt, "", task.fingerprint, task.updateOnly)
			if err := saveBootstrapTaskState(opts.ProjectPath, state); err != nil {
				return err
			}
			activeConflictPaths[task.index] = append([]string(nil), task.conflictPaths...)
			active++
			jobs <- task
		}
		return nil
	}
	if err := dispatch(); err != nil {
		close(jobs)
		close(impactJobs)
		return BatchValidateResult{}, err
	}

	for active > 0 || impactActive > 0 || len(queue) > 0 || len(conflictQueue) > 0 {
		if fatalErr != nil && active == 0 && impactActive == 0 {
			break
		}
		if active == 0 && impactActive == 0 && (len(queue) > 0 || len(conflictQueue) > 0) {
			if err := dispatch(); err != nil {
				fatalErr = err
			}
		}
		select {
		case item := <-results:
			active--
			delete(activeConflictPaths, item.task.index)
			source := sources[item.task.index]
			intentToRemove := ""
			var conflict sourceConflictError
			var syncErr postCommitSyncError
			switch {
			case errors.As(item.err, &conflict):
				if item.task.conflictAttempts >= maxConflictAttempts {
					fatalErr = SourceTaskExhaustedError{SourcePath: source, Attempts: item.task.conflictAttempts, Err: item.err}
					state.Sources[source] = updateBootstrapSourceState(state.Sources[source], "failed", item.task.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
				} else {
					paths := sortedUniqueStrings(append(item.task.conflictPaths, conflict.Paths...))
					next := sourceTask{index: item.task.index, attempt: item.task.attempt, force: true, updateOnly: item.task.updateOnly, fingerprint: item.task.fingerprint, conflictAttempts: item.task.conflictAttempts + 1, conflictPaths: paths}
					conflictQueue = append(conflictQueue, next)
					updated := updateBootstrapSourceState(state.Sources[source], "conflicted", item.task.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
					updated.ConflictAttempts, updated.ConflictPaths = next.conflictAttempts, append([]string(nil), paths...)
					state.Sources[source] = updated
					emitSchedulerProgress(opts, sources, next, "conflict_requeued", item.err.Error())
				}
			case errors.As(item.err, &syncErr):
				queue = append(queue, sourceTask{index: item.task.index, attempt: item.task.attempt, syncOnly: true, syncResult: syncErr.Result, updateOnly: item.task.updateOnly, fingerprint: item.task.fingerprint})
				state.Sources[source] = updateBootstrapSourceState(state.Sources[source], "pg_pending", item.task.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
				emitSchedulerProgress(opts, sources, item.task, "pg_pending", item.err.Error())
			case item.err != nil:
				if isTransientSourceTaskError(item.err) {
					next := sourceTask{index: item.task.index, attempt: item.task.attempt, force: true, updateOnly: item.task.updateOnly, fingerprint: item.task.fingerprint, conflictAttempts: item.task.conflictAttempts, conflictPaths: append([]string(nil), item.task.conflictPaths...)}
					if isConflictTask(next) {
						conflictQueue = append(conflictQueue, next)
						updated := updateBootstrapSourceState(state.Sources[source], "conflicted", next.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
						updated.ConflictAttempts, updated.ConflictPaths = next.conflictAttempts, append([]string(nil), next.conflictPaths...)
						state.Sources[source] = updated
						emitSchedulerProgress(opts, sources, next, "transient_conflict_requeued", item.err.Error())
					} else {
						queue = append(queue, next)
						state.Sources[source] = updateBootstrapSourceState(state.Sources[source], "transient", item.task.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
						emitSchedulerProgress(opts, sources, next, "transient_requeued", item.err.Error())
					}
				} else if item.task.attempt >= attemptLimit(item.task.updateOnly) {
					if fatalErr == nil {
						fatalErr = SourceTaskExhaustedError{SourcePath: source, Attempts: item.task.attempt, Err: item.err}
					}
					state.Sources[source] = updateBootstrapSourceState(state.Sources[source], "failed", item.task.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
				} else {
					next := sourceTask{index: item.task.index, attempt: item.task.attempt + 1, force: true, updateOnly: item.task.updateOnly, fingerprint: item.task.fingerprint, conflictAttempts: item.task.conflictAttempts, conflictPaths: append([]string(nil), item.task.conflictPaths...)}
					if isConflictTask(next) {
						conflictQueue = append(conflictQueue, next)
						updated := updateBootstrapSourceState(state.Sources[source], "conflicted", next.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
						updated.ConflictAttempts, updated.ConflictPaths = next.conflictAttempts, append([]string(nil), next.conflictPaths...)
						state.Sources[source] = updated
						emitSchedulerProgress(opts, sources, next, "failed_conflict_requeued", item.err.Error())
					} else {
						queue = append(queue, next)
						state.Sources[source] = updateBootstrapSourceState(state.Sources[source], "invalidated", item.task.attempt, item.err.Error(), item.task.fingerprint, item.task.updateOnly)
						emitSchedulerProgress(opts, sources, next, "failed_requeued", item.err.Error())
					}
				}
			default:
				settled[item.task.index] = item.result
				settledState := updateBootstrapSourceState(state.Sources[source], "settled", item.task.attempt, "", item.task.fingerprint, false)
				settledState.ConflictAttempts, settledState.ConflictPaths = 0, nil
				state.Sources[source] = settledState
				intentToRemove = item.result.CommitIntent
				if item.task.syncOnly && opts.OnProgress != nil {
					opts.OnProgress(ValidateProgress{
						Phase: "completed", SourcePath: source, Index: item.task.index + 1, Total: len(sources),
						Files: len(item.result.Files), Reviews: item.result.ReviewCount, Attempt: item.task.attempt,
					})
				}
				if assessor != nil && len(item.result.PageChanges) > 0 {
					job := buildImpactJob(opts, sources, source, item.result, settled)
					if len(job.input.Candidates) > 0 {
						impactActive++
						emitSchedulerProgress(opts, sources, item.task, "impact_review", fmt.Sprintf("checking %d completed source(s)", len(job.input.Candidates)))
						if cached, ok := state.ImpactDecisions[job.id]; ok {
							impactResults <- impactJobResult{job: job, decision: cached}
						} else {
							pending := []string{source}
							for candidate := range job.candidates {
								pending = append(pending, candidate)
							}
							state.PendingImpacts[job.id] = pending
							impactJobs <- job
						}
					}
				}
			}
			stateErr := saveBootstrapTaskState(opts.ProjectPath, state)
			if stateErr != nil {
				if fatalErr == nil {
					fatalErr = stateErr
				}
			} else if intentToRemove != "" {
				_ = os.Remove(intentToRemove)
			}
			if err := dispatch(); err != nil && fatalErr == nil {
				fatalErr = err
			}
		case assessed := <-impactResults:
			impactActive--
			if assessed.err != nil {
				if isTransientSourceTaskError(assessed.err) {
					assessed.job.transientAttempts++
					impactActive++
					pending := []string{assessed.job.input.ChangedSource}
					for candidate := range assessed.job.candidates {
						pending = append(pending, candidate)
					}
					state.PendingImpacts[assessed.job.id] = sortedUniqueStrings(pending)
					impactJobs <- assessed.job
				} else if fatalErr == nil {
					delete(state.PendingImpacts, assessed.job.id)
					fatalErr = fmt.Errorf("assess wiki impact: %w", assessed.err)
				}
			} else {
				delete(state.PendingImpacts, assessed.job.id)
				state.ImpactDecisions[assessed.job.id] = assessed.decision
				for _, finding := range assessed.decision.Affected {
					index, ok := assessed.job.candidates[finding.SourcePath]
					if !ok {
						continue
					}
					current, done := settled[index]
					if !done {
						continue
					}
					delete(settled, index)
					previous := state.Sources[finding.SourcePath]
					if previous.ImpactAttempts >= maxImpactAttempts || previous.Attempts >= attemptLimit(true) {
						fatalErr = SourceTaskExhaustedError{SourcePath: finding.SourcePath, Attempts: previous.Attempts, Err: fmt.Errorf("impact re-integration required: %s", finding.Reason)}
						failed := updateBootstrapSourceState(previous, "failed", previous.Attempts, finding.Reason, previous.InputFingerprint, true)
						state.Sources[finding.SourcePath] = failed
						continue
					}
					next := sourceTask{index: index, attempt: previous.Attempts + 1, force: true, updateOnly: true, fingerprint: previous.InputFingerprint}
					queue = append(queue, next)
					invalidated := updateBootstrapSourceState(previous, "invalidated", previous.Attempts, finding.Reason, previous.InputFingerprint, true)
					invalidated.ImpactAttempts++
					state.Sources[finding.SourcePath] = invalidated
					emitSchedulerProgress(opts, sources, next, "impact_requeued", finding.Reason)
					_ = current
				}
			}
			if err := saveBootstrapTaskState(opts.ProjectPath, state); err != nil && fatalErr == nil {
				fatalErr = err
			}
			if err := dispatch(); err != nil && fatalErr == nil {
				fatalErr = err
			}
		}
		if fatalErr != nil && active == 0 && impactActive == 0 {
			break
		}
	}
	close(jobs)
	close(impactJobs)
	if fatalErr != nil {
		return batchFromSettled(settled), fatalErr
	}
	return batchFromSettled(settled), nil
}

func isTransientSourceTaskError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || isPersistedTransientSourceError(fmt.Sprint(err))
}

func isPersistedTransientSourceError(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	for _, fragment := range []string{
		"context deadline exceeded",
		"client.timeout exceeded",
		"i/o timeout",
		"connection reset by peer",
		"unexpected eof",
		// Some OpenAI-compatible gateways multiplex Codex-backed ChatGPT
		// accounts with ordinary API backends. A single routed account may
		// reject a model that other accounts in the same gateway successfully
		// serve. Treat this account-routing response like a transport failure so
		// it does not spend the source's semantic failure budget.
		"model is not supported when using codex with a chatgpt account",
	} {
		if strings.Contains(reason, fragment) {
			return true
		}
	}
	return false
}

func normalizedLLMConcurrency(taskConcurrency, configured int) int {
	if configured > 0 {
		return configured
	}
	if taskConcurrency > 0 {
		return taskConcurrency
	}
	return 1
}

func isConflictTask(task sourceTask) bool {
	return task.conflictAttempts > 0 || len(task.conflictPaths) > 0
}

func updateBootstrapSourceState(previous bootstrapSourceTask, status string, attempts int, reason, fingerprint string, updateOnly bool) bootstrapSourceTask {
	previous.Status = status
	previous.Attempts = attempts
	previous.Reason = reason
	previous.InputFingerprint = fingerprint
	previous.UpdateOnly = updateOnly
	return previous
}

func sourceTaskInputFingerprint(sourceSHA, contract string, maxAttempts, maxConflictAttempts, maxImpactAttempts int) string {
	return textSHA256(fmt.Sprintf("source-task-v4\nsource=%s\ncontract=%s\nmax_attempts=%d\nmax_conflict_attempts=%d\nmax_impact_attempts=%d", sourceSHA, contract, maxAttempts, maxConflictAttempts, maxImpactAttempts))
}

func buildImpactJob(opts ValidateOptions, sources []string, changedSource string, changed ValidateResult, settled map[int]ValidateResult) impactJob {
	changedPaths := map[string]bool{}
	for _, change := range changed.PageChanges {
		changedPaths[change.Path] = true
	}
	candidates := map[string]int{}
	var inputs []ImpactCandidate
	for index, result := range settled {
		source := sources[index]
		if source == changedSource || !resultTouchesPaths(result, changedPaths) {
			continue
		}
		data, _ := readSourceExcerpt(source, 64<<10)
		inputs = append(inputs, ImpactCandidate{
			SourcePath: source, SourceTitle: inferSourceTitle(source, string(data)), RawPath: result.RawPath,
			Files: append([]string(nil), result.Files...), SourceExcerpt: string(data),
		})
		candidates[source] = index
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].SourcePath < inputs[j].SourcePath })
	idInput := "impact-policy-v2\n" + changedSource
	for _, change := range changed.PageChanges {
		idInput += "\n" + change.Path + ":" + change.BeforeSHA256 + ":" + change.AfterSHA256
	}
	for _, candidate := range inputs {
		idInput += "\n" + candidate.SourcePath
	}
	return impactJob{
		id: textSHA256(idInput), candidates: candidates,
		input: ImpactInput{
			ProjectPath: opts.ProjectPath, ChangedSource: changedSource,
			Purpose: readOptional(filepath.Join(opts.ProjectPath, "purpose.md")),
			Schema:  readOptional(filepath.Join(opts.ProjectPath, "schema.md")),
			Changes: append([]PageChange(nil), changed.PageChanges...), Candidates: inputs,
		},
	}
}

func resultTouchesPaths(result ValidateResult, paths map[string]bool) bool {
	for _, path := range result.Files {
		if paths[path] {
			return true
		}
	}
	for _, path := range result.Dependencies {
		if paths[path] {
			return true
		}
	}
	return false
}

func emitSchedulerProgress(opts ValidateOptions, sources []string, task sourceTask, phase, reason string) {
	if opts.OnProgress == nil {
		return
	}
	opts.OnProgress(ValidateProgress{
		Phase: phase, SourcePath: sources[task.index], Index: task.index + 1, Total: len(sources),
		Attempt: task.attempt, Error: reason, Reason: reason,
	})
}

func batchFromSettled(settled map[int]ValidateResult) BatchValidateResult {
	indexes := make([]int, 0, len(settled))
	for index := range settled {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var batch BatchValidateResult
	for _, index := range indexes {
		result := settled[index]
		batch.Results = append(batch.Results, result)
		batch.SourceCount++
		batch.FileCount += filesWritten(result)
		batch.ReviewCount += reviewsWritten(result)
		batch.SkippedCount += skippedCount(result)
	}
	return batch
}

func loadBootstrapTaskState(projectPath string) (bootstrapTaskState, error) {
	state := bootstrapTaskState{Version: 1, Sources: map[string]bootstrapSourceTask{}, PendingImpacts: map[string][]string{}, ImpactDecisions: map[string]ImpactDecision{}}
	data, err := os.ReadFile(bootstrapTaskStatePath(projectPath))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return bootstrapTaskState{}, fmt.Errorf("read bootstrap task state: %w", err)
	}
	if state.Sources == nil {
		state.Sources = map[string]bootstrapSourceTask{}
	}
	if state.PendingImpacts == nil {
		state.PendingImpacts = map[string][]string{}
	}
	if state.ImpactDecisions == nil {
		state.ImpactDecisions = map[string]ImpactDecision{}
	}
	return state, nil
}

func saveBootstrapTaskState(projectPath string, state bootstrapTaskState) error {
	path := bootstrapTaskStatePath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func bootstrapTaskStatePath(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "bootstrap-state.json")
}

func writeBootstrapCommitIntent(projectPath, sourcePath string, files []FileBlock) (string, error) {
	intent := bootstrapCommitIntent{SourcePath: sourcePath}
	for _, file := range files {
		intent.Paths = append(intent.Paths, file.Path)
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(projectPath, ".kbcore", "bootstrap-intents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, textSHA256(sourceManifestKey(sourcePath))+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func loadBootstrapCommitIntents(projectPath string) ([]bootstrapCommitIntent, error) {
	dir := filepath.Join(projectPath, ".kbcore", "bootstrap-intents")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var intents []bootstrapCommitIntent
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var intent bootstrapCommitIntent
		if err := json.Unmarshal(data, &intent); err != nil {
			return nil, fmt.Errorf("read bootstrap commit intent %s: %w", entry.Name(), err)
		}
		intents = append(intents, intent)
	}
	return intents, nil
}
