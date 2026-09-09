// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/reqctx"
	"github.com/mdhender/bricolage/internal/service"
)

// The invitation and user-lookup routes (issue #6).
//
// These are additions to DESIGN.md 12's API surface, which listed
// POST /users/{uid}/roles and nothing else about users. That route needs a uid
// and nothing could produce one, so the collection it acts on is here too: an
// API that can give a role to a user nobody can find is incomplete, and earl is
// the measure of that.
//
// Four routes for invitations and no more. There is deliberately nothing that
// extends an invitation, renews an expired one, or forces one to expire; the
// reasoning is in internal/service/invitations.go, beside the code that would
// have to implement them. Re-inviting supersedes, and revoking settles.
//
// Revocation is POST .../revoke rather than DELETE .../invitations/{uid},
// which is a departure from how this package spells every other destructive
// act. The row is retained -- it is an audit record -- so a DELETE that left it
// in place would be the one DELETE in this API that does not delete, and the
// UI's form would have to spell it differently anyway (an HTML form may only
// GET or POST). One spelling for both transports, and the mapping table in
// internal/server has an identity row rather than a translation.

// invitationResponse is one invitation as the API speaks it.
//
// There is no token and no hash in it, and there is no route that would produce
// one: the link exists in the response to the POST that created it and nowhere
// else, because what is stored is a SHA-256 and there is nothing to retrieve.
type invitationResponse struct {
	UID   string `json:"uid"`
	Email string `json:"email"`

	// Status is the stored status, and Expired is derived. Both are here
	// because they answer different questions: a pending invitation that has
	// lapsed is still pending -- nothing runs at the 48-hour mark to write
	// anything down -- and a client drawing a work queue needs to know which
	// of its pending rows are dead.
	Status  string `json:"status"`
	Expired bool   `json:"expired"`

	// Redeemable is the one answer the redemption check gives, computed at the
	// instant of this response. It is what the UI draws a row's actions from.
	Redeemable bool `json:"redeemable"`

	ExpiresAt time.Time  `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	SettledAt *time.Time `json:"settled_at,omitempty"`

	Reason string `json:"reason,omitempty"`

	InvitedBy     string `json:"invited_by,omitempty"`
	InvitedByName string `json:"invited_by_name,omitempty"`

	// User is the account a redemption created, absent until one has.
	User string `json:"user,omitempty"`
}

func newInvitationResponse(inv domain.Invitation, now time.Time) invitationResponse {
	out := invitationResponse{
		UID:           inv.UID,
		Email:         inv.Email,
		Status:        string(inv.Status),
		Expired:       inv.Expired(now),
		Redeemable:    inv.Redeemable(now),
		ExpiresAt:     inv.ExpiresAt,
		CreatedAt:     inv.CreatedAt,
		Reason:        inv.Reason,
		InvitedBy:     inv.InvitedByUID,
		InvitedByName: inv.InvitedByName,
		User:          inv.UserUID,
	}
	if !inv.SettledAt.IsZero() {
		at := inv.SettledAt
		out.SettledAt = &at
	}
	return out
}

// createdInvitationResponse is POST /api/v1/invitations, and it is the only
// place a link is ever rendered.
//
// The link is shown once, the way "cmsdb bootstrap admin" shows a generated
// password once. There is no e-mail transport yet (#4), so the administrator
// sends it by hand; when there is one, this response stops being the only way
// it reaches anybody and still does no harm.
type createdInvitationResponse struct {
	Invitation invitationResponse `json:"invitation"`

	// Link is the absolute URL to put in front of a person, and Token is the
	// credential inside it, for a client that would rather build its own. Both
	// appear here and never again.
	Link  string `json:"link"`
	Token string `json:"token"`
}

// invitationsResponse is GET /api/v1/invitations.
type invitationsResponse struct {
	// Status is the filter that produced this list, "all" when there was none,
	// so that a client can tell an empty pending list from an empty database.
	Status      string               `json:"status"`
	Invitations []invitationResponse `json:"invitations"`
}

// invitationRequest is the body of POST /api/v1/invitations.
type invitationRequest struct {
	Email string `json:"email"`
}

// revokeRequest is the body of POST /api/v1/invitations/{uid}/revoke. The
// reason is optional and is why there is no second verb for forcing an
// invitation to expire: the two acts differ only in what is recorded.
type revokeRequest struct {
	Reason string `json:"reason"`
}

// redemptionRequest is the body of POST /api/v1/invitations/redemption.
//
// The token is in the body and not in the path, deliberately: a path is logged
// by every proxy in front of this one, and the link that carries it in a path
// is already as much exposure as this credential should get.
type redemptionRequest struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// listInvitations is GET /api/v1/invitations?status=pending|all|...
//
// Pending by default, which is what makes the list a work queue rather than an
// archive of everything ever sent. "all" is the escape hatch, and the four
// statuses narrow it.
func (h *Handler) listInvitations(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	status := domain.InvitationPending
	switch raw := r.URL.Query().Get("status"); raw {
	case "":
		// The default.
	case "all":
		status = ""
	default:
		parsed, err := domain.ParseInvitationStatus(raw)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		status = parsed
	}

	list, err := h.svc.Invitations(r.Context(), identity, status)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	now := h.svc.Now()
	out := invitationsResponse{
		Status:      string(status),
		Invitations: make([]invitationResponse, 0, len(list)),
	}
	if out.Status == "" {
		out.Status = "all"
	}
	for _, inv := range list {
		out.Invitations = append(out.Invitations, newInvitationResponse(inv, now))
	}
	writeJSON(w, http.StatusOK, out)
}

// showInvitation is GET /api/v1/invitations/{uid}.
func (h *Handler) showInvitation(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	inv, err := h.svc.Invitation(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newInvitationResponse(inv, h.svc.Now()))
}

// createInvitation is POST /api/v1/invitations.
//
// Inviting an address that already has a pending invitation is not a conflict:
// the new invitation supersedes the old one and the old link stops working.
// That is how a lapsed invitation is replaced, and it is why there is no renew.
func (h *Handler) createInvitation(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req invitationRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	created, err := h.svc.CreateInvitation(r.Context(), identity, service.NewInvitation{Email: req.Email})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, createdInvitationResponse{
		Invitation: newInvitationResponse(created.Invitation, h.svc.Now()),
		Link:       created.Link,
		Token:      created.Token,
	})
}

// revokeInvitation is POST /api/v1/invitations/{uid}/revoke.
//
// It answers 200 both times. Revoking a revoked invitation changed nothing and
// is not a conflict, which is the answer approving twice and reading a
// notification twice both give. Revoking a redeemed one is a 409, because the
// account exists and this operation would not remove it.
func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req revokeRequest
	if err := decodeJSONOptional(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	inv, err := h.svc.RevokeInvitation(r.Context(), identity, r.PathValue("uid"), req.Reason)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newInvitationResponse(inv, h.svc.Now()))
}

// redeemInvitation is POST /api/v1/invitations/redemption: the one
// unauthenticated write in this package besides POST /sessions.
//
// It issues no session. A successful redemption answers with the account and
// nothing else, and the person signs in with the password they just set --
// which is what keeps this route from having login CSRF (issue #6). A client
// that wants a session calls POST /sessions next, like everybody else.
//
// Every failure is the same 422 with the same detail. That is not laziness: six
// causes distinguished would make this route an oracle for which addresses have
// been invited, and the e-mail check that stops a forwarded link from working
// would become the way to enumerate them. Which of the six it was is in the
// server's log.
func (h *Handler) redeemInvitation(w http.ResponseWriter, r *http.Request) {
	var req redemptionRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	u, err := h.svc.Redeem(r.Context(), domain.Redemption{
		Token:    req.Token,
		Email:    req.Email,
		Name:     req.Name,
		Password: req.Password,
	}, reqctx.ClientAddr(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newUserResponse(u))
}

// usersResponse is GET /api/v1/users. The rows are me.go's userResponse, which
// is the shape GET /me already speaks: one account, one rendering.
type usersResponse struct {
	Users []userResponse `json:"users"`
}

// listUsers is GET /api/v1/users?q=substring.
//
// It exists so that POST /users/{uid}/roles is usable by somebody who did not
// keep the output of the command that created the account. Reading the list of
// accounts is a system-wide question with no document to scope it to, so it
// needs read over the system subject -- the pattern the job queue follows.
func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	list, err := h.svc.Users(r.Context(), identity, r.URL.Query().Get("q"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := usersResponse{Users: make([]userResponse, 0, len(list))}
	for _, u := range list {
		out.Users = append(out.Users, newUserResponse(u))
	}
	writeJSON(w, http.StatusOK, out)
}

// showUser is GET /api/v1/users/{uid}.
func (h *Handler) showUser(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	u, err := h.svc.User(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newUserResponse(u))
}
