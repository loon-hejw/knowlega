package service

import (
	"context"
	"encoding/json"
	"time"

	runnerv1 "github.com/hejw/qm-backend/api/qm/runner/v1"
	"github.com/hejw/qm-backend/internal/data"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type RunnerService struct {
	runnerv1.UnimplementedRunnerServiceServer
	runs      *data.RunRepository
	leaseTTL  time.Duration
	maxClaims int
}

func NewRunnerService(runs *data.RunRepository, leaseTTL time.Duration, maxClaims int) *RunnerService {
	return &RunnerService{runs: runs, leaseTTL: leaseTTL, maxClaims: maxClaims}
}

func (s *RunnerService) EnqueueRun(ctx context.Context, request *runnerv1.EnqueueRunRequest) (*runnerv1.EnqueueRunReply, error) {
	id, deduped, err := s.runs.Enqueue(ctx, request.GetSessionId(), json.RawMessage(request.GetPayload()), request.GetIdempotencyKey(), int(request.GetMaxAttempts()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &runnerv1.EnqueueRunReply{RunId: id, Deduped: deduped}, nil
}

func (s *RunnerService) GetRun(ctx context.Context, request *runnerv1.GetRunRequest) (*runnerv1.GetRunReply, error) {
	run, err := s.runs.Get(ctx, request.GetRunId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return runSnapshotReply(run), nil
}

func (s *RunnerService) GetActiveRunForSession(ctx context.Context, request *runnerv1.GetActiveRunForSessionRequest) (*runnerv1.GetRunReply, error) {
	run, err := s.runs.Active(ctx, request.GetSessionId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return runSnapshotReply(run), nil
}

func (s *RunnerService) ListActiveSessionIDs(ctx context.Context, _ *runnerv1.ListActiveSessionIDsRequest) (*runnerv1.ListActiveSessionIDsReply, error) {
	ids, err := s.runs.ActiveSessionIDs(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &runnerv1.ListActiveSessionIDsReply{SessionIds: ids}, nil
}

func runSnapshotReply(run *data.RunSnapshot) *runnerv1.GetRunReply {
	if run == nil {
		return &runnerv1.GetRunReply{}
	}
	reply := &runnerv1.RunSnapshot{
		RunId:             run.ID,
		SessionId:         run.SessionID,
		Status:            run.Status,
		Payload:           run.Payload,
		IdempotencyKey:    run.IdempotencyKey,
		Attempts:          run.Attempts,
		ErrorAttempts:     run.ErrorAttempts,
		MaxAttempts:       run.MaxAttempts,
		LeaseToken:        run.LeaseToken,
		WorkerId:          run.WorkerID,
		CreatedAtUnixMs:   run.CreatedAt,
		HasResult:         run.Result != nil,
		HasDeliveryState:  run.DeliveryState != nil,
		HasLeaseExpiresAt: run.LeaseExpiresAt != nil,
		HasStartedAt:      run.StartedAt != nil,
		HasFinishedAt:     run.FinishedAt != nil,
	}
	if run.Result != nil {
		reply.Result = run.Result
	}
	if run.DeliveryState != nil {
		reply.DeliveryState = run.DeliveryState
	}
	if run.LeaseExpiresAt != nil {
		reply.LeaseExpiresAtUnixMs = *run.LeaseExpiresAt
	}
	if run.StartedAt != nil {
		reply.StartedAtUnixMs = *run.StartedAt
	}
	if run.FinishedAt != nil {
		reply.FinishedAtUnixMs = *run.FinishedAt
	}
	return &runnerv1.GetRunReply{Found: true, Run: reply}
}

func (s *RunnerService) ClaimRun(ctx context.Context, request *runnerv1.ClaimRunRequest) (*runnerv1.ClaimRunReply, error) {
	run, err := s.runs.Claim(ctx, request.GetRunnerId(), s.leaseTTL)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if run == nil {
		return &runnerv1.ClaimRunReply{}, nil
	}
	return &runnerv1.ClaimRunReply{RunId: run.ID, LeaseToken: run.LeaseToken, Payload: run.Payload, SessionId: run.SessionID, Attempt: run.Attempt, LeaseExpiresAtUnixMs: run.LeaseExpiresAt, MaxAttempts: run.MaxAttempts, ErrorAttempts: run.ErrorAttempts, CreatedAtUnixMs: run.CreatedAt, StartedAtUnixMs: run.StartedAt}, nil
}

func (s *RunnerService) ClaimRunByID(ctx context.Context, request *runnerv1.ClaimRunByIDRequest) (*runnerv1.ClaimRunReply, error) {
	run, err := s.runs.ClaimByID(ctx, request.GetRunId(), request.GetRunnerId(), s.leaseTTL)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if run == nil {
		return &runnerv1.ClaimRunReply{}, nil
	}
	return &runnerv1.ClaimRunReply{RunId: run.ID, LeaseToken: run.LeaseToken, Payload: run.Payload, SessionId: run.SessionID, Attempt: run.Attempt, LeaseExpiresAtUnixMs: run.LeaseExpiresAt, MaxAttempts: run.MaxAttempts, ErrorAttempts: run.ErrorAttempts, CreatedAtUnixMs: run.CreatedAt, StartedAtUnixMs: run.StartedAt}, nil
}

func (s *RunnerService) HeartbeatRun(ctx context.Context, request *runnerv1.HeartbeatRunRequest) (*runnerv1.HeartbeatRunReply, error) {
	ok, err := s.runs.Heartbeat(ctx, request.GetRunId(), request.GetLeaseToken(), s.leaseTTL)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &runnerv1.HeartbeatRunReply{Accepted: ok}, nil
}

func (s *RunnerService) ReleaseRunLease(ctx context.Context, request *runnerv1.ReleaseRunLeaseRequest) (*runnerv1.ReleaseRunLeaseReply, error) {
	ok, err := s.runs.ReleaseLease(ctx, request.GetRunId(), request.GetLeaseToken())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &runnerv1.ReleaseRunLeaseReply{Accepted: ok}, nil
}

func (s *RunnerService) FailRun(ctx context.Context, request *runnerv1.FailRunRequest) (*runnerv1.FailRunReply, error) {
	ok, requeued, err := s.runs.Fail(ctx, request.GetRunId(), request.GetLeaseToken(), request.GetReason(), request.GetRetry())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &runnerv1.FailRunReply{Accepted: ok, Requeued: requeued}, nil
}

func (s *RunnerService) AppendRunEvent(ctx context.Context, request *runnerv1.AppendRunEventRequest) (*runnerv1.AppendRunEventReply, error) {
	ok, err := s.runs.AppendEvent(ctx, request.GetRunId(), request.GetLeaseToken(), request.GetSequence(), request.GetIdempotencyKey(), json.RawMessage(request.GetPayload()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &runnerv1.AppendRunEventReply{Accepted: ok}, nil
}

func (s *RunnerService) CompleteRun(ctx context.Context, request *runnerv1.CompleteRunRequest) (*runnerv1.CompleteRunReply, error) {
	ok, err := s.runs.Complete(ctx, request.GetRunId(), request.GetLeaseToken(), request.GetIdempotencyKey(), json.RawMessage(request.GetResult()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &runnerv1.CompleteRunReply{Accepted: ok}, nil
}

func (s *RunnerService) SignalRun(ctx context.Context, request *runnerv1.SignalRunRequest) (*runnerv1.SignalRunReply, error) {
	ok, err := s.runs.Signal(ctx, request.GetRunId(), json.RawMessage(request.GetPayload()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &runnerv1.SignalRunReply{Accepted: ok}, nil
}

func (s *RunnerService) TakePendingSignals(ctx context.Context, request *runnerv1.TakePendingSignalsRequest) (*runnerv1.TakePendingSignalsReply, error) {
	signals, err := s.runs.TakePendingSignals(ctx, request.GetRunId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	reply := &runnerv1.TakePendingSignalsReply{Signals: make([]*runnerv1.RunSignal, 0, len(signals))}
	for _, signal := range signals {
		reply.Signals = append(reply.Signals, &runnerv1.RunSignal{Id: signal.ID, Kind: signal.Kind, Text: signal.Text, Payload: signal.Payload, CreatedAtUnixMs: signal.CreatedAt})
	}
	return reply, nil
}

func (s *RunnerService) PendingSignalRunIDs(ctx context.Context, _ *runnerv1.PendingSignalRunIDsRequest) (*runnerv1.PendingSignalRunIDsReply, error) {
	ids, err := s.runs.PendingSignalRunIDs(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &runnerv1.PendingSignalRunIDsReply{RunIds: ids}, nil
}

func (s *RunnerService) PruneSignals(ctx context.Context, request *runnerv1.PruneSignalsRequest) (*runnerv1.PruneSignalsReply, error) {
	deleted, err := s.runs.PruneSignals(ctx, request.GetOlderThanUnixMs())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &runnerv1.PruneSignalsReply{Deleted: deleted}, nil
}

func (s *RunnerService) AppendRunActivity(ctx context.Context, request *runnerv1.AppendRunActivityRequest) (*runnerv1.AppendRunActivityReply, error) {
	activity := request.GetActivity()
	if activity == nil {
		return nil, status.Error(codes.InvalidArgument, "activity is required")
	}
	var parent *int64
	if activity.GetHasParentSequence() {
		value := activity.GetParentSequence()
		parent = &value
	}
	ok, err := s.runs.AppendActivity(ctx, request.GetRunId(), data.RunActivity{Sequence: activity.GetSequence(), ParentSequence: parent, Type: activity.GetType(), Payload: activity.GetPayload(), CreatedAt: activity.GetCreatedAtUnixMs()})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &runnerv1.AppendRunActivityReply{Accepted: ok}, nil
}

func (s *RunnerService) ListRunActivity(ctx context.Context, request *runnerv1.ListRunActivityRequest) (*runnerv1.ListRunActivityReply, error) {
	activities, err := s.runs.ListActivity(ctx, request.GetRunId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	reply := &runnerv1.ListRunActivityReply{Activities: make([]*runnerv1.RunActivity, 0, len(activities))}
	for _, activity := range activities {
		entry := &runnerv1.RunActivity{Sequence: activity.Sequence, Type: activity.Type, Payload: activity.Payload, CreatedAtUnixMs: activity.CreatedAt}
		if activity.ParentSequence != nil {
			entry.HasParentSequence = true
			entry.ParentSequence = *activity.ParentSequence
		}
		reply.Activities = append(reply.Activities, entry)
	}
	return reply, nil
}

func (s *RunnerService) PruneRunActivity(ctx context.Context, request *runnerv1.PruneRunActivityRequest) (*runnerv1.PruneRunActivityReply, error) {
	deleted, err := s.runs.PruneActivity(ctx, request.GetOlderThanUnixMs())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &runnerv1.PruneRunActivityReply{Deleted: deleted}, nil
}

func (s *RunnerService) ReapExpiredRuns(ctx context.Context, request *runnerv1.ReapExpiredRunsRequest) (*runnerv1.ReapExpiredRunsReply, error) {
	maxAge := time.Duration(request.GetMaxAgeMs()) * time.Millisecond
	events, err := s.runs.ReapExpired(ctx, maxAge, s.maxClaims)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	reply := &runnerv1.ReapExpiredRunsReply{Events: make([]*runnerv1.ReapEvent, 0, len(events))}
	for _, event := range events {
		if event.Outcome == "requeued" {
			reply.Requeued++
		} else {
			reply.Parked++
		}
		reply.Events = append(reply.Events, &runnerv1.ReapEvent{RunId: event.RunID, SessionId: event.SessionID, WorkerId: event.WorkerID, Attempts: event.Attempts, ErrorAttempts: event.ErrorAttempts, Outcome: event.Outcome})
	}
	return reply, nil
}
