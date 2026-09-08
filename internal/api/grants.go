// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"fmt"
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// grantRequest is the body of POST /api/v1/grants: a privilege, for a role,
// over a scope.
//
// The scope fields are pointers because NULL is the wildcard (DESIGN.md 7): an
// omitted field means "everywhere", and a present one means "here". A zero
// value would be indistinguishable from an omitted one, and the difference
// between them is the difference between a grant over one site and a grant
// over all of them.
type grantRequest struct {
	Role      string `json:"role"`
	Privilege string `json:"privilege"`

	Site         *int64  `json:"site,omitempty"`
	DocKind      *string `json:"doc_kind,omitempty"`
	State        *string `json:"state,omitempty"`
	Category     *int64  `json:"category,omitempty"`
	CategoryPath *string `json:"category_path,omitempty"`
	CategoryDeep *bool   `json:"category_deep,omitempty"`
	Workflow     *int64  `json:"workflow,omitempty"`
	Collection   *int64  `json:"collection,omitempty"`
	Document     *int64  `json:"document,omitempty"`
}

// createGrant is POST /api/v1/grants.
//
// The refusal that matters here is the escalation one: a caller may not create
// a grant conferring a privilege they do not themselves hold over that scope
// (DESIGN.md 7.3, invariant 12). It is enforced in the service, in the same
// function that writes the row, and it arrives here as domain.ErrForbidden,
// which statusFor turns into 403. Nothing is written on that path.
func (h *Handler) createGrant(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req grantRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if req.Role == "" {
		h.writeError(w, r, fmt.Errorf("role: required: %w", domain.ErrInvalid))
		return
	}
	privilege, err := domain.ParsePrivilege(req.Privilege)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	// category_deep defaults to true, which is the schema's default and the
	// behaviour a person expects of a grant on a category.
	deep := true
	if req.CategoryDeep != nil {
		deep = *req.CategoryDeep
	}

	g, err := h.svc.CreateGrant(r.Context(), identity, service.GrantRequest{
		RoleSlug:  req.Role,
		Privilege: privilege,
		Scope: domain.Scope{
			SiteID:       req.Site,
			DocKind:      req.DocKind,
			State:        req.State,
			CategoryID:   req.Category,
			CategoryPath: req.CategoryPath,
			CategoryDeep: deep,
			WorkflowID:   req.Workflow,
			CollectionID: req.Collection,
			DocumentID:   req.Document,
		},
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, grantResponse{
		Privilege:   g.Privilege.String(),
		Scope:       newScopeResponse(g.Scope),
		Description: g.Scope.String(),
	})
}

// assignRoleRequest is the body of POST /api/v1/users/{uid}/roles.
type assignRoleRequest struct {
	Role string `json:"role"`
}

// assignRole is POST /api/v1/users/{uid}/roles.
//
// The same escalation rule applies as to writing a grant, and for the same
// reason: handing somebody a role hands them every grant it carries, so the
// actor must be able to confer each of them. Without that, "you may not give
// away what you do not have" is true of grants and false of the roles that
// hold them, which is the same hole with one more step in it.
func (h *Handler) assignRole(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req assignRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if req.Role == "" {
		h.writeError(w, r, fmt.Errorf("role: required: %w", domain.ErrInvalid))
		return
	}

	if err := h.svc.AssignRole(r.Context(), identity, r.PathValue("uid"), req.Role); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
