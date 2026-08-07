package biz

import (
	"context"
	"errors"
	"strings"
)

type Project struct {
	ID        string   `json:"id"`
	OrgID     string   `json:"orgId"`
	Name      string   `json:"name"`
	OwnerID   string   `json:"ownerId"`
	MemberIDs []string `json:"memberIds"`
	CreatedAt int64    `json:"createdAt"`
	UpdatedAt int64    `json:"updatedAt"`
}

type ProjectMutation struct {
	Status  string
	Project *Project
	Changed bool
}

type ProjectRepo interface {
	Create(context.Context, string, string) (*Project, error)
	ListForMember(context.Context, string) ([]Project, error)
	AddMember(context.Context, string, string, string) (ProjectMutation, error)
	RemoveMember(context.Context, string, string, string) (ProjectMutation, error)
	Rename(context.Context, string, string, string) (ProjectMutation, error)
	HasScopeMembership(context.Context, string, string) (bool, error)
}

type ProjectUsecase struct{ repo ProjectRepo }

func NewProjectUsecase(repo ProjectRepo) *ProjectUsecase { return &ProjectUsecase{repo: repo} }

func (u *ProjectUsecase) Create(ctx context.Context, principalID, name string) (*Project, error) {
	if principalID == "" || normalizeName(name) == "" {
		return nil, errors.New("principalId and name required")
	}
	return u.repo.Create(ctx, principalID, normalizeName(name))
}

func (u *ProjectUsecase) List(ctx context.Context, principalID string) ([]Project, error) {
	if principalID == "" {
		return nil, errors.New("principalId required")
	}
	return u.repo.ListForMember(ctx, principalID)
}

func (u *ProjectUsecase) AddMember(ctx context.Context, projectID, actorID, memberID string) (ProjectMutation, error) {
	if actorID == "" || memberID == "" {
		return ProjectMutation{Status: "invalid_member"}, nil
	}
	return u.repo.AddMember(ctx, projectID, actorID, memberID)
}

func (u *ProjectUsecase) RemoveMember(ctx context.Context, projectID, actorID, memberID string) (ProjectMutation, error) {
	if actorID == "" || memberID == "" {
		return ProjectMutation{Status: "invalid_member"}, nil
	}
	return u.repo.RemoveMember(ctx, projectID, actorID, memberID)
}

func (u *ProjectUsecase) Rename(ctx context.Context, projectID, actorID, name string) (ProjectMutation, error) {
	if actorID == "" || normalizeName(name) == "" {
		return ProjectMutation{Status: "invalid_name"}, nil
	}
	return u.repo.Rename(ctx, projectID, actorID, normalizeName(name))
}

func ProjectScopeID(id string) string { return "group:web-project-" + id }

func normalizeName(value string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(value), " "))[:min(len(strings.TrimSpace(strings.Join(strings.Fields(value), " "))), 200)]
}
