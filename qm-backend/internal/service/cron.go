package service

import (
	"context"
	"encoding/json"

	cronv1 "github.com/hejw/qm-backend/api/qm/cron/v1"
	"github.com/hejw/qm-backend/internal/data"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type CronService struct {
	cronv1.UnimplementedCronServiceServer
	crons *data.CronRepository
}

func NewCronService(crons *data.CronRepository) *CronService { return &CronService{crons: crons} }

func (s *CronService) Get(ctx context.Context, request *cronv1.GetRequest) (*cronv1.GetReply, error) {
	cron, err := s.crons.Get(ctx, request.GetId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if cron == nil {
		return &cronv1.GetReply{}, nil
	}
	return &cronv1.GetReply{Found: true, Cron: toCron(cron)}, nil
}

func (s *CronService) List(ctx context.Context, _ *cronv1.ListRequest) (*cronv1.ListReply, error) {
	crons, err := s.crons.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	reply := &cronv1.ListReply{Crons: make([]*cronv1.Cron, 0, len(crons))}
	for i := range crons {
		reply.Crons = append(reply.Crons, toCron(&crons[i]))
	}
	return reply, nil
}

func (s *CronService) Put(ctx context.Context, request *cronv1.PutRequest) (*cronv1.PutReply, error) {
	cron, err := s.crons.Put(ctx, request.GetId(), json.RawMessage(request.GetPayload()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &cronv1.PutReply{Cron: toCron(cron)}, nil
}

func (s *CronService) PutIfAbsent(ctx context.Context, request *cronv1.PutIfAbsentRequest) (*cronv1.PutReply, error) {
	cron, err := s.crons.PutIfAbsent(ctx, request.GetId(), json.RawMessage(request.GetPayload()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &cronv1.PutReply{Cron: toCron(cron)}, nil
}

func (s *CronService) Merge(ctx context.Context, request *cronv1.MergeRequest) (*cronv1.MergeReply, error) {
	cron, err := s.crons.Merge(ctx, request.GetId(), json.RawMessage(request.GetPatch()), request.GetRemoveKeys())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if cron == nil {
		return &cronv1.MergeReply{}, nil
	}
	return &cronv1.MergeReply{Found: true, Cron: toCron(cron)}, nil
}

func (s *CronService) Delete(ctx context.Context, request *cronv1.DeleteRequest) (*cronv1.DeleteReply, error) {
	if err := s.crons.Delete(ctx, request.GetId()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cronv1.DeleteReply{}, nil
}

func (s *CronService) Take(ctx context.Context, request *cronv1.TakeRequest) (*cronv1.TakeReply, error) {
	cron, err := s.crons.Take(ctx, request.GetId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if cron == nil {
		return &cronv1.TakeReply{}, nil
	}
	return &cronv1.TakeReply{Found: true, Cron: toCron(cron)}, nil
}

func (s *CronService) ListDue(ctx context.Context, request *cronv1.ListDueRequest) (*cronv1.ListDueReply, error) {
	crons, err := s.crons.ListDue(ctx, request.GetNowUnixMs(), int(request.GetLimit()))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	reply := &cronv1.ListDueReply{Crons: make([]*cronv1.Cron, 0, len(crons))}
	for _, cron := range crons {
		var next int64
		if cron.NextFireAt != nil {
			next = *cron.NextFireAt
		}
		reply.Crons = append(reply.Crons, &cronv1.Cron{Id: cron.ID, Payload: cron.JSON, NextFireAtUnixMs: next})
	}
	return reply, nil
}

func (s *CronService) UnclaimSlot(ctx context.Context, request *cronv1.UnclaimSlotRequest) (*cronv1.UnclaimSlotReply, error) {
	var prior *int64
	if request.GetHasPriorLastFiredAt() {
		value := request.GetPriorLastFiredAtUnixMs()
		prior = &value
	}
	restored, err := s.crons.UnclaimSlot(ctx, request.GetId(), request.GetScheduledAtUnixMs(), request.GetFiredAtUnixMs(), prior)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cronv1.UnclaimSlotReply{Restored: restored}, nil
}

func (s *CronService) RecordFire(ctx context.Context, request *cronv1.RecordFireRequest) (*cronv1.RecordFireReply, error) {
	updated, err := s.crons.RecordFire(ctx, request.GetId(), json.RawMessage(request.GetEntry()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &cronv1.RecordFireReply{Updated: updated}, nil
}

func (s *CronService) ClaimSlot(ctx context.Context, request *cronv1.ClaimSlotRequest) (*cronv1.ClaimSlotReply, error) {
	var next *int64
	if request.GetHasNextFireAt() {
		value := request.GetNextFireAtUnixMs()
		next = &value
	}
	claimed, err := s.crons.ClaimSlot(ctx, request.GetId(), request.GetScheduledAtUnixMs(), request.GetFiredAtUnixMs(), next)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cronv1.ClaimSlotReply{Claimed: claimed}, nil
}

func (s *CronService) MarkFired(ctx context.Context, request *cronv1.MarkFiredRequest) (*cronv1.MarkFiredReply, error) {
	var next *int64
	if request.GetHasNextFireAt() {
		value := request.GetNextFireAtUnixMs()
		next = &value
	}
	updated, err := s.crons.MarkFired(ctx, request.GetId(), request.GetFiredAtUnixMs(), next)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cronv1.MarkFiredReply{Updated: updated}, nil
}

func toCron(record *data.CronRecord) *cronv1.Cron {
	if record == nil {
		return nil
	}
	var next int64
	if record.NextFireAt != nil {
		next = *record.NextFireAt
	}
	return &cronv1.Cron{Id: record.ID, Payload: record.JSON, NextFireAtUnixMs: next}
}
