// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
)

// userResponse is a user as the API speaks it: the uid and nothing internal
// (invariant 10). The password hash is not a field here and never will be.
type userResponse struct {
	UID       string    `json:"uid"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

func newUserResponse(u domain.User) userResponse {
	return userResponse{
		UID:       u.UID,
		Email:     u.Email,
		Name:      u.Name,
		Active:    u.Active,
		CreatedAt: u.CreatedAt,
	}
}

// roleResponse is a role as the API speaks it. The slug is the external
// identifier; the integer key never leaves the process.
type roleResponse struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// scopeResponse is a grant's scope. Every field is omitted when it is a
// wildcard, so the JSON reads the way the rule does: what is stated is what is
// constrained.
type scopeResponse struct {
	Site         *int64  `json:"site,omitempty"`
	DocKind      *string `json:"doc_kind,omitempty"`
	Category     *string `json:"category,omitempty"`
	CategoryDeep bool    `json:"category_deep,omitempty"`
	Workflow     *int64  `json:"workflow,omitempty"`
	State        *string `json:"state,omitempty"`
	Collection   *int64  `json:"collection,omitempty"`
	Document     *int64  `json:"document,omitempty"`
}

// grantResponse is one grant: what it confers and where.
type grantResponse struct {
	Privilege string        `json:"privilege"`
	Scope     scopeResponse `json:"scope"`

	// Description is Scope.String: "everything", or the constraints stated.
	// It is here because the CLI prints it and because a person reading a
	// grant list wants the sentence, not the fields.
	Description string `json:"description"`
}

// meResponse is GET /api/v1/me.
//
// It carries the roles and the grants, not just the user. That is what makes
// PLAN.md M2 acceptance 10 assertable from outside the process: a session
// issued by the development route must carry exactly what a password login
// carries, and the way to check is to compare this document from both.
type meResponse struct {
	User      userResponse    `json:"user"`
	Roles     []roleResponse  `json:"roles"`
	Grants    []grantResponse `json:"grants"`
	ExpiresAt time.Time       `json:"session_expires_at"`
}

// me is GET /api/v1/me.
func (h *Handler) me(w http.ResponseWriter, _ *http.Request, identity domain.Identity) {
	writeJSON(w, http.StatusOK, newMeResponse(identity))
}

func newMeResponse(identity domain.Identity) meResponse {
	out := meResponse{
		User:      newUserResponse(identity.User),
		Roles:     make([]roleResponse, 0, len(identity.Roles)),
		Grants:    make([]grantResponse, 0, len(identity.Grants)),
		ExpiresAt: identity.Session.ExpiresAt,
	}
	for _, r := range identity.Roles {
		out.Roles = append(out.Roles, roleResponse{Slug: r.Slug, Name: r.Name})
	}
	for _, g := range identity.Grants {
		out.Grants = append(out.Grants, grantResponse{
			Privilege:   g.Privilege.String(),
			Scope:       newScopeResponse(g.Scope),
			Description: g.Scope.String(),
		})
	}
	return out
}

func newScopeResponse(s domain.Scope) scopeResponse {
	return scopeResponse{
		Site:         s.SiteID,
		DocKind:      s.DocKind,
		Category:     s.CategoryPath,
		CategoryDeep: s.CategoryID != nil && s.CategoryDeep,
		Workflow:     s.WorkflowID,
		State:        s.State,
		Collection:   s.CollectionID,
		Document:     s.DocumentID,
	}
}
