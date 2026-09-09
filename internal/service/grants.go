// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// GrantRequest is a grant somebody is asking to create: a privilege, for a
// role, over a scope.
type GrantRequest struct {
	RoleSlug  string
	Privilege domain.Privilege
	Scope     domain.Scope

	// CategoryPath is the category the grant is constrained to, named by its
	// path. It is separate from Scope because a scope carries both the id and
	// the path and the two must agree: the resolver matches a subtree by
	// prefix on the path and performs no I/O (DESIGN.md 7.2), so a grant whose
	// path named a different row from its id would match the wrong documents
	// with nothing to notice. Taking one and resolving the other here means
	// there is one source for the pair.
	CategoryPath string
}

// CreateGrant writes a grant, refusing an escalation (DESIGN.md 7.3,
// invariant 12).
//
// This is the grant-writing path the invariant names, and the check is in it
// rather than beside it. The system we learned from added the rule in 1.8.0,
// after discovering that a System Administrator could add themselves to Global
// Admins; the reason it was possible for six years is that the check lived in
// the screens rather than in the write.
//
// The refusal wraps domain.ErrForbidden, so the edge answers 403 through the
// one mapping function it has, and nothing is written: the check happens
// before the INSERT, not after it in a transaction somebody may forget to roll
// back.
func (s *Service) CreateGrant(ctx context.Context, actor domain.Identity, req GrantRequest) (domain.Grant, error) {
	if req.CategoryPath != "" {
		if req.Scope.SiteID == nil {
			return domain.Grant{}, fmt.Errorf(
				"a category constraint needs a site: %q names one row per site: %w",
				req.CategoryPath, domain.ErrInvalid)
		}
		c, err := s.db.CategoryByPath(ctx, *req.Scope.SiteID, req.CategoryPath)
		if err != nil {
			return domain.Grant{}, err
		}
		req.Scope.CategoryID = &c.ID
		req.Scope.CategoryPath = &c.Path
	}
	if err := req.Scope.Validate(); err != nil {
		return domain.Grant{}, err
	}
	if err := authz.CanGrant(actor.Grants, req.Scope, req.Privilege); err != nil {
		return domain.Grant{}, err
	}

	role, err := s.db.RoleBySlug(ctx, req.RoleSlug)
	if err != nil {
		return domain.Grant{}, err
	}

	now := s.Now()
	g, err := s.db.CreateGrant(ctx, domain.Grant{
		RoleID:    role.ID,
		Privilege: req.Privilege,
		Scope:     req.Scope,
		CreatedAt: now,
		CreatedBy: actor.User.ID,
	})
	if err != nil {
		return domain.Grant{}, err
	}

	if _, err := s.db.RecordEvent(ctx, domain.Event{
		Type:        events.GrantCreated,
		ActorID:     actor.User.ID,
		SubjectKind: "grant",
		SubjectID:   g.ID,
		Payload: map[string]any{
			"role":      role.Slug,
			"privilege": g.Privilege.String(),
			"scope":     g.Scope.String(),
		},
		OccurredAt: now,
	}); err != nil {
		return domain.Grant{}, err
	}
	return g, nil
}

// AssignRole gives a user a role and records it.
//
// The same escalation rule applies in spirit: handing somebody a role hands
// them every grant it carries, so the actor must be able to confer each of
// them. Checking grant by grant is what makes "you may not give away what you
// do not have" true of role assignment as well as of grant creation.
func (s *Service) AssignRole(ctx context.Context, actor domain.Identity, userUID, roleSlug string) error {
	role, err := s.db.RoleBySlug(ctx, roleSlug)
	if err != nil {
		return err
	}
	u, err := s.db.UserByUID(ctx, userUID)
	if err != nil {
		return err
	}

	carried, err := s.db.GrantsForRole(ctx, role.ID)
	if err != nil {
		return err
	}
	for _, g := range carried {
		if err := authz.CanGrant(actor.Grants, g.Scope, g.Privilege); err != nil {
			return fmt.Errorf("role %q carries %s over %s: %w", role.Slug, g.Privilege, g.Scope, err)
		}
	}

	if err := s.db.AssignRole(ctx, u.ID, role.ID); err != nil {
		return err
	}

	now := s.Now()
	_, err = s.db.RecordEvent(ctx, domain.Event{
		Type:        events.RoleAssigned,
		ActorID:     actor.User.ID,
		SubjectKind: domain.SubjectUser,
		SubjectID:   u.ID,
		Payload:     map[string]any{"role": role.Slug, "user": u.UID, "email": u.Email},
		OccurredAt:  now,
	})
	return err
}
