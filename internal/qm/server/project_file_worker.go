package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	agentservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func ProcessProjectFileScope(ctx context.Context, projectID string, ref knowlega.ScopeRef, memberships *data.ProjectFileMembershipRepository, scopes *data.KnowledgeScopeRepository, agent *knowlega.Agent, runLLMReview, recovering bool) (result agentservice.MaintainWikiResult, resultErr error) {
	defer func() {
		if _, err := RefreshKnowledgeScopeStatus(ctx, ref, scopes, agent); err != nil {
			if resultErr == nil {
				resultErr = err
			} else {
				slog.Error("knowledge worker refresh scope status", "scope", ref.ExternalScopeID, "error", err)
			}
		}
	}()
	if recovering {
		if _, err := agent.RecoverIngestQueue(ctx, ref); err != nil {
			return agentservice.MaintainWikiResult{}, err
		}
		if err := memberships.RequeueProcessing(ctx, projectID); err != nil {
			return agentservice.MaintainWikiResult{}, err
		}
	}
	claimed, err := memberships.ClaimRunnableProcessing(ctx, projectID)
	if err != nil {
		return agentservice.MaintainWikiResult{}, err
	}
	if len(claimed) == 0 {
		status, err := agent.EnsureScope(ctx, ref)
		if err != nil {
			return agentservice.MaintainWikiResult{}, err
		}
		if !projectFileMaintenanceRunnable(claimed, status) {
			if recovering {
				if _, err := agent.RecoverMaintenance(ref); err != nil {
					return agentservice.MaintainWikiResult{}, err
				}
			}
			now := time.Now().UTC()
			return agentservice.MaintainWikiResult{Status: "ok", StartedAt: now, FinishedAt: now}, nil
		}
	}
	result, resultErr = agent.Maintain(ctx, ref, runLLMReview)
	ReconcileProjectFileMemberships(ctx, projectID, ref, memberships, agent)
	if resultErr != nil {
		if err := memberships.MarkClaimedFailed(ctx, claimed, resultErr.Error()); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("record project file maintenance failure: %w", err))
		}
	}
	return result, resultErr
}

func projectFileMaintenanceRunnable(claimed []data.ProjectFileMembership, status knowlega.ScopeStatus) bool {
	return len(claimed) > 0 || status.Queue.Pending > 0 || status.Queue.Failed > 0 || status.State == agentservice.KnowledgeWorkspaceQueued
}

// RefreshKnowledgeScopeStatus mirrors the filesystem-derived state into both
// Knowledge Core's scope binding and QM's operational scope registry.
func RefreshKnowledgeScopeStatus(ctx context.Context, ref knowlega.ScopeRef, scopes *data.KnowledgeScopeRepository, agent *knowlega.Agent) (knowlega.ScopeStatus, error) {
	status, err := agent.RefreshScope(ctx, ref)
	if err != nil {
		return knowlega.ScopeStatus{}, err
	}
	if scopes == nil {
		return status, nil
	}
	_, err = scopes.Ensure(ctx, data.KnowledgeScope{
		OrgID:           ref.OrgID,
		ExternalScopeID: ref.ExternalScopeID,
		Kind:            ref.Kind,
		ProjectID:       status.ProjectID,
		ProjectName:     ref.Name,
		RootPath:        status.ProjectPath,
		Status:          status.State,
	})
	return status, err
}

func ReconcileProjectFileMemberships(ctx context.Context, projectID string, ref knowlega.ScopeRef, memberships *data.ProjectFileMembershipRepository, agent *knowlega.Agent) {
	items, err := memberships.ListByProject(ctx, projectID)
	if err != nil {
		slog.Error("knowledge worker list project files", "project", projectID, "error", err)
		return
	}
	for _, item := range items {
		if item.QueueTaskID == nil || item.Status == "unsupported" || item.Status == "removing" {
			continue
		}
		task, found, err := agent.IngestTask(ref, *item.QueueTaskID)
		if err != nil || !found {
			if err != nil {
				message := err.Error()
				_ = memberships.SetState(ctx, item.ProjectID, item.FileID, "failed", nil, nil, 0, &message)
			} else {
				message := "knowledge ingest task is missing"
				_ = memberships.SetState(ctx, item.ProjectID, item.FileID, "failed", item.RawPath, item.QueueTaskID, item.GeneratedPageCount, &message)
			}
			continue
		}
		status := "queued"
		var lastError *string
		switch task.Status {
		case agentservice.IngestTaskProcessing:
			status = "processing"
		case agentservice.IngestTaskDone:
			status = "ready"
			sourcePath := task.RawPath
			if sourcePath == "" && item.RawPath != nil {
				sourcePath = *item.RawPath
			}
			if err := agent.BindQMFile(ref, sourcePath, item.FileID, item.ProjectID, item.SourceSHA256); err != nil {
				status = "failed"
				message := err.Error()
				lastError = &message
			} else if err := agent.SyncSourceManifest(ctx, ref); err != nil {
				status = "failed"
				message := err.Error()
				lastError = &message
			}
		case agentservice.IngestTaskFailed:
			status = "failed"
			lastError = &task.Error
		}
		var rawPath *string
		if task.RawPath != "" {
			rawPath = &task.RawPath
		}
		if err := memberships.SetState(ctx, item.ProjectID, item.FileID, status, rawPath, &task.ID, len(task.Files), lastError); err != nil {
			slog.Error("knowledge worker update project file", "project", projectID, "file", item.FileID, "error", err)
		}
	}
}
