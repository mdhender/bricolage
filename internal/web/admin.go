// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// The administration screen: the processes documents move through, the
// structure they are filed against, and the two writes that hand out access.
//
// Both writes carry the anti-escalation refusal, and neither restates it here:
// nobody may grant a privilege they do not hold over that scope, and handing
// somebody a role hands them every grant it carries (invariant 12,
// DESIGN.md 7.3). The check is in internal/service, where both transports
// reach it.
//
// There is no screen that edits a workflow. The state machine is written down
// once, in the migration that seeds it (DESIGN.md 6), and a screen that could
// rewrite it would be a screen that could leave documents in states their own
// workflow no longer declares.

// adminPage is GET /admin.
type adminPage struct {
	Base

	Workflows    []domain.Workflow
	Sites        []domain.Site
	ElementTypes []domain.ElementType
	Channels     []domain.OutputChannel
	Privileges   []string
	Problem      string
}

func (h *Handler) admin(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	p := adminPage{
		Base:       h.base(r, identity, "Administration"),
		Privileges: privilegeNames(),
		Problem:    r.URL.Query().Get("problem"),
	}

	workflows, err := h.svc.Workflows(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.Workflows = workflows

	// The structure panels are what a person needs beside the grant form: a
	// grant names a site and a category path, and typing either from memory
	// is how a grant ends up matching nothing.
	if sites, err := h.svc.Sites(r.Context(), identity); err == nil {
		p.Sites = sites
		for _, s := range sites {
			if channels, err := h.svc.OutputChannels(r.Context(), identity, s.ID); err == nil {
				p.Channels = append(p.Channels, channels...)
			}
		}
	}
	if types, err := h.svc.ElementTypes(r.Context()); err == nil {
		p.ElementTypes = types
	}

	h.render(w, r, "admin.gohtml", http.StatusOK, p)
}

// createGrant is POST /admin/grants: give a role a privilege over a scope.
func (h *Handler) createGrant(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	privilege, err := domain.ParsePrivilege(r.PostFormValue("privilege"))
	if err != nil {
		h.fail(w, r, err)
		return
	}

	req := service.GrantRequest{
		RoleSlug:  strings.TrimSpace(r.PostFormValue("role")),
		Privilege: privilege,
	}

	// Every scope dimension is optional, and an empty box means "not
	// constrained by this" rather than zero: a grant scoped to site 0 would
	// be a grant that matches nothing while looking like one that matches
	// everything.
	if raw := strings.TrimSpace(r.PostFormValue("site")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			h.fail(w, r, fmt.Errorf("site: %q is not a site: %w", raw, domain.ErrInvalid))
			return
		}
		req.Scope.SiteID = &id
	}
	if raw := strings.TrimSpace(r.PostFormValue("doc_kind")); raw != "" {
		req.Scope.DocKind = &raw
	}
	if raw := strings.TrimSpace(r.PostFormValue("state")); raw != "" {
		req.Scope.State = &raw
	}
	if raw := strings.TrimSpace(r.PostFormValue("category")); raw != "" {
		// The category is named by its path, which is what a person reads and
		// what the grant prefix-matches against. The service resolves it to a
		// row: a client that sent both the id and the path would be a client
		// that could send two that disagree (DESIGN.md 12).
		req.CategoryPath = raw
	}
	req.Scope.CategoryDeep = r.PostFormValue("deep") != ""

	if _, err := h.svc.CreateGrant(r.Context(), identity, req); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/admin", "granted")
}

// assignRole is POST /admin/roles: give a user a role.
func (h *Handler) assignRole(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	user := strings.TrimSpace(r.PostFormValue("user"))
	role := strings.TrimSpace(r.PostFormValue("role"))
	if err := h.svc.AssignRole(r.Context(), identity, user, role); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/admin", "role assigned")
}

// privilegeNames lists the privileges a grant may carry, in the order
// internal/domain declares them.
func privilegeNames() []string {
	out := make([]string, 0, len(domain.Privileges))
	for _, p := range domain.Privileges {
		out = append(out, p.String())
	}
	return out
}
