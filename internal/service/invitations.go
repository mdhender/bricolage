// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// The invitation use cases (issue #6).
//
// Registration is invite-only and there is no self-service account creation.
// An administrator creates an invitation for an address; the system returns a
// link once; the person at the other end redeems it, which creates the account
// and leaves them at the login form.
//
// There are two verbs and deliberately no others. An administrator may create
// an invitation and revoke one. They may not extend a pending invitation, renew
// an expired one, or force one to expire, and the reasons are worth having here
// beside the code rather than only in the issue:
//
//   - Extending saves nothing and costs the bound. The administrator cannot see
//     the token, so extending is a decision taken blind about a credential that
//     may be sitting in a forwarded mail; and the invitee has to be told the
//     window moved either way, so what it saves is pasting a link into a message
//     that has to be sent regardless. With extension, "48 hours, single use"
//     stops being a property of the system and becomes a property of an
//     administrator's habits.
//   - Renewing is already spelled "invite again". CreateInvitation supersedes
//     the old row, mints a new token, and starts a fresh 48 hours; the lapsed
//     row stays as its own audit record rather than being edited into a live
//     one.
//   - Forcing expiry is revoking with a different word in the audit trail,
//     which is what RevokeInvitation's reason is for.

// InvitationAdmin is what creating or revoking an invitation needs, over the
// system subject. It is domain.Create for the reason AlertAdmin and
// ElementTypeAdmin are: an invitation belongs to no site and no category, so
// only a grant constraining nothing matches it.
const InvitationAdmin = domain.Create

// UserRead is what listing users needs, over the system subject.
//
// Reading the list of accounts is a system-wide question with no document to
// scope it to, which is the pattern the job queue follows. It is deliberately
// lower than InvitationAdmin: somebody who has to hand work to a person needs
// their uid, and needing the privilege that creates accounts to look one up
// would make assignment an administrator's errand.
const UserRead = domain.Read

// NewInvitation is an invitation somebody is asking for.
type NewInvitation struct {
	Email string
}

// CreatedInvitation is a new invitation and the link that redeems it.
//
// The link exists in this struct and nowhere else, ever again. What the database
// holds is a SHA-256, so there is nothing to retrieve afterwards and no route
// that could retrieve it -- which is why the transports show it once, the way
// "cmsdb bootstrap admin" shows a generated password once.
type CreatedInvitation struct {
	Invitation domain.Invitation

	// Token is the credential in the link; Link is the absolute URL to put in
	// front of a person. Both are returned once.
	Token string
	Link  string
}

// Invitations lists invitations, by default only the pending ones.
//
// An empty status means every one of them, which is what the administrator's
// "show all" asks for. Pending by default is not a performance concession: a
// list that accretes every invitation ever sent stops being a work queue, and
// these rows are never deleted.
func (s *Service) Invitations(ctx context.Context, actor domain.Identity, status domain.InvitationStatus) ([]domain.Invitation, error) {
	if err := s.mayAdministerInvitations(actor, "invitations"); err != nil {
		return nil, err
	}
	list, err := s.db.ListInvitations(ctx, status)
	if err != nil {
		return nil, err
	}
	return withoutHashes(list), nil
}

// Invitation reads one invitation.
func (s *Service) Invitation(ctx context.Context, actor domain.Identity, uid string) (domain.Invitation, error) {
	if err := s.mayAdministerInvitations(actor, fmt.Sprintf("invitation %q", uid)); err != nil {
		return domain.Invitation{}, err
	}
	inv, err := s.db.InvitationByUID(ctx, uid)
	if err != nil {
		return domain.Invitation{}, err
	}
	inv.TokenHash = ""
	return inv, nil
}

// CreateInvitation writes an invitation and returns the link, once.
//
// An address that already has a pending invitation is not an error: the new one
// supersedes it, in the same transaction, and the old link stops working at
// once. That is what re-inviting is, and it is the answer to the three verbs
// this package does not have -- a lapsed invitation is replaced rather than
// revived, because the hash of the old one may already have been cleared and
// reviving it would mean minting a token, which is this method.
//
// An address that already has an account is refused. The invitation would
// create a second user row with the same address and fail on users.email's
// UNIQUE index at redemption, which is a failure at the worst possible moment:
// the person reading it is not the person who could fix it.
func (s *Service) CreateInvitation(ctx context.Context, actor domain.Identity, in NewInvitation) (CreatedInvitation, error) {
	if err := s.mayAdministerInvitations(actor, "invitations"); err != nil {
		return CreatedInvitation{}, err
	}
	email := domain.NormalizeEmail(in.Email)
	if err := domain.ValidateEmail(email); err != nil {
		return CreatedInvitation{}, err
	}

	if _, err := s.db.UserByEmail(ctx, email); err == nil {
		return CreatedInvitation{}, fmt.Errorf(
			"invitation for %q: that address already has an account: %w", email, domain.ErrConflict)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return CreatedInvitation{}, err
	}

	now := s.Now()
	token, hash, err := authz.NewToken()
	if err != nil {
		return CreatedInvitation{}, err
	}
	uid, err := ids.New(now)
	if err != nil {
		return CreatedInvitation{}, err
	}

	inv := domain.Invitation{
		UID:       uid,
		Email:     email,
		TokenHash: hash,
		Status:    domain.InvitationPending,
		ExpiresAt: now.Add(domain.InvitationTTL),
		InvitedBy: actor.User.ID,
		CreatedAt: now,
	}
	created, err := s.db.CreateInvitation(ctx, inv,
		domain.Event{
			Type:       events.InvitationCreated,
			ActorID:    actor.User.ID,
			Payload:    map[string]any{"uid": uid, "email": email, "expires_at": inv.ExpiresAt},
			OccurredAt: now,
		},
		domain.Event{
			Type:       events.InvitationSuperseded,
			ActorID:    actor.User.ID,
			Payload:    map[string]any{"email": email, "superseded_by": uid},
			OccurredAt: now,
		})
	if err != nil {
		return CreatedInvitation{}, err
	}
	created.TokenHash = ""

	s.log.Info("invitation created",
		"invitation", created.UID, "email", email, "by", actor.User.UID,
		"expires_at", created.ExpiresAt)

	return CreatedInvitation{
		Invitation: created,
		Token:      token,
		Link:       s.origin.InvitationLink(token),
	}, nil
}

// RevokeInvitation cancels a pending invitation, lapsed or not.
//
// Revoking a lapsed one is the important case and not an edge case: it is how an
// address becomes invitable again by hand, and it is the only way to settle a
// row that time has killed but nothing has recorded -- nothing runs at the
// 48-hour mark. Re-inviting settles it too, which is the path an administrator
// usually takes.
//
// Revoking twice is not an error, which is the rule approving and withdrawing
// already follow: the second call changes nothing, so there is nothing to
// report as a conflict. Revoking a redeemed invitation is refused, and the
// refusal says why -- the account exists, and taking it away is a different
// operation that this one must not be mistaken for.
func (s *Service) RevokeInvitation(ctx context.Context, actor domain.Identity, uid, reason string) (domain.Invitation, error) {
	if err := s.mayAdministerInvitations(actor, fmt.Sprintf("invitation %q", uid)); err != nil {
		return domain.Invitation{}, err
	}
	inv, err := s.db.InvitationByUID(ctx, uid)
	if err != nil {
		return domain.Invitation{}, err
	}
	switch inv.Status {
	case domain.InvitationRevoked, domain.InvitationSuperseded:
		// Already settled, and settled the same way as far as anybody
		// redeeming is concerned: the link does not work and no account came
		// of it. Report it as it stands.
		inv.TokenHash = ""
		return inv, nil
	case domain.InvitationRedeemed:
		return domain.Invitation{}, fmt.Errorf(
			"invitation %q was redeemed on %s and the account exists; revoking it would not remove that account: %w",
			uid, inv.SettledAt.Format("2006-01-02"), domain.ErrConflict)
	}

	now := s.Now()
	out, settled, err := s.db.SettleInvitation(ctx, inv.ID, domain.InvitationRevoked,
		strings.TrimSpace(reason), now, domain.Event{
			Type:    events.InvitationRevoked,
			ActorID: actor.User.ID,
			Payload: map[string]any{
				"uid": inv.UID, "email": inv.Email, "reason": strings.TrimSpace(reason),
			},
			OccurredAt: now,
		})
	if err != nil {
		return domain.Invitation{}, err
	}
	if settled {
		s.log.Info("invitation revoked",
			"invitation", out.UID, "email", out.Email, "by", actor.User.UID, "reason", reason)
	}
	out.TokenHash = ""
	return out, nil
}

// ErrRedemptionRefused is the one answer every redemption failure gets.
//
// Unknown token, wrong address, lapsed, already redeemed, revoked, superseded:
// six causes, one response. Distinguishing them would make the form an oracle
// for "does this person have an account here", and would turn the
// anti-forwarding check into a way to enumerate who was invited. The cause goes
// to the log at Info, because an operator debugging a genuine failure needs it
// and the person on the other end must not have it.
//
// It wraps domain.ErrInvalid, so both transports answer 422 "Invalid request"
// through the one status table (internal/edge) -- the same status for all six,
// since a 404 for an unknown token and a 409 for a used one would distinguish
// them just as well as two messages would.
//
// It is exported so that a transport can tell it from the refusals that follow
// it. A password too short and a missing name are the caller's own mistakes,
// reported only to somebody who has already proved they hold the link and know
// the address -- so those may say what is wrong, and a person who cannot be told
// which rule their password broke cannot choose one that passes.
var ErrRedemptionRefused = fmt.Errorf("that invitation cannot be accepted: %w", domain.ErrInvalid)

// Redeem creates the account an invitation was for.
//
// It issues no session, deliberately. The user is left at the login form and
// signs in with the password they just set. CSRF needs an ambient credential
// and redemption has none, so the ordinary attack does not apply -- but a
// redemption that issued a session would have login CSRF: an attacker holding
// an invitation could make a victim's browser redeem it with an
// attacker-chosen password, leaving the victim working inside an account the
// attacker can read later. Ending at the login form removes that surface
// entirely, and it exercises the password while the person still remembers
// typing it.
//
// The address is required and is what makes a forwarded link useless to
// whoever received it: holding the link is not enough, you also have to know
// which address was invited. It is compared in constant time, because the one
// thing this route must not become is a way to confirm an address a byte at a
// time.
func (s *Service) Redeem(ctx context.Context, in domain.Redemption, clientAddr string) (domain.User, error) {
	email := domain.NormalizeEmail(in.Email)
	name := strings.TrimSpace(in.Name)

	refuse := func(why string, args ...any) (domain.User, error) {
		// One log line, naming which of the six it was, and one response that
		// names none of them.
		s.log.Info("invitation redemption refused",
			"reason", fmt.Sprintf(why, args...), "email", email, "client", clientAddr)
		return domain.User{}, ErrRedemptionRefused
	}

	if in.Token == "" || email == "" {
		return refuse("the form was incomplete")
	}

	inv, err := s.db.InvitationByTokenHash(ctx, authz.HashToken(in.Token))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return refuse("no invitation has that token")
		}
		return domain.User{}, err
	}

	now := s.Now()

	// The order here is deliberate and it is the reason a lapsed row's hash
	// being uncleared is harmless: expiry is checked before the address is
	// compared and before anything is written, so the hash on a dead row is
	// already inert.
	if inv.Status != domain.InvitationPending {
		return refuse("invitation %s is %s", inv.UID, inv.Status)
	}
	if inv.Expired(now) {
		// Touched, so clear the hash on the way past. This is the
		// opportunistic half of the clearing rule: nothing runs at the
		// 48-hour mark, so this is the only moment a lapsed row gets.
		if err := s.db.ClearInvitationToken(ctx, inv.ID); err != nil {
			s.log.Warn("could not clear the token of a lapsed invitation",
				"invitation", inv.UID, "error", err)
		}
		return refuse("invitation %s expired at %s", inv.UID, inv.ExpiresAt.Format("2006-01-02T15:04:05Z"))
	}
	if subtle.ConstantTimeCompare([]byte(inv.Email), []byte(email)) != 1 {
		return refuse("invitation %s was for another address", inv.UID)
	}

	// From here the failures are the caller's own and may say so: a name they
	// did not give and a password too short are not facts about who was
	// invited, and a person who cannot be told which rule their password broke
	// cannot choose one that passes.
	if name == "" {
		return domain.User{}, fmt.Errorf("name: required: %w", domain.ErrInvalid)
	}
	hash, err := authz.HashPassword(in.Password)
	if err != nil {
		return domain.User{}, err
	}

	uid, err := ids.New(now)
	if err != nil {
		return domain.User{}, err
	}

	u, redeemed, err := s.db.Redeem(ctx, inv.ID, store.NewUser{
		UID:          uid,
		Email:        inv.Email,
		Name:         name,
		PasswordHash: hash,
		CreatedAt:    now,
	},
		domain.Event{
			Type: events.InvitationRedeemed,
			Payload: map[string]any{
				"uid": inv.UID, "email": inv.Email, "user": uid, "client": clientAddr,
			},
			OccurredAt: now,
		},
		domain.Event{
			Type:       events.UserCreated,
			Payload:    map[string]any{"uid": uid, "email": inv.Email, "name": name, "invitation": inv.UID},
			OccurredAt: now,
		})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// Lost the race with another redemption or a revocation. Same
			// answer as every other failure.
			return refuse("invitation %s was settled by somebody else first", inv.UID)
		}
		return domain.User{}, err
	}

	s.log.Info("invitation redeemed",
		"invitation", redeemed.UID, "user", u.UID, "email", u.Email, "client", clientAddr)
	return u, nil
}

// Users lists accounts, optionally narrowed by a substring of the address or
// the name.
//
// This is the other half of what issue #6 delivers, and it is what makes the
// role-assignment route usable: it needs a uid, and before this there was no
// way to obtain one except to have kept the output of the command that created
// the account.
func (s *Service) Users(ctx context.Context, actor domain.Identity, q string) ([]domain.User, error) {
	if !authz.Allows(actor.Grants, systemSubject(), UserRead) {
		return nil, fmt.Errorf("users: %s over the system is required: %w", UserRead, domain.ErrForbidden)
	}
	list, err := s.db.ListUsers(ctx, strings.TrimSpace(q))
	if err != nil {
		return nil, err
	}
	return withoutPasswords(list), nil
}

// User reads one account by uid.
func (s *Service) User(ctx context.Context, actor domain.Identity, uid string) (domain.User, error) {
	if !authz.Allows(actor.Grants, systemSubject(), UserRead) {
		return domain.User{}, fmt.Errorf("user %q: %s over the system is required: %w",
			uid, UserRead, domain.ErrForbidden)
	}
	u, err := s.db.UserByUID(ctx, uid)
	if err != nil {
		return domain.User{}, err
	}
	u.PasswordHash = ""
	return u, nil
}

func (s *Service) mayAdministerInvitations(actor domain.Identity, what string) error {
	if authz.Allows(actor.Grants, systemSubject(), InvitationAdmin) {
		return nil
	}
	return fmt.Errorf("%s: %s over the system is required: %w", what, InvitationAdmin, domain.ErrForbidden)
}

// withoutHashes strips the token hash from everything on its way out of this
// package. Nothing above the service has any use for it, and the way to be sure
// it is never serialised is for it never to be there.
func withoutHashes(list []domain.Invitation) []domain.Invitation {
	for i := range list {
		list[i].TokenHash = ""
	}
	return list
}

// withoutPasswords does the same for the stored bcrypt hash, which never leaves
// the store except on its way into the verifier (DESIGN.md 14, "Logging").
func withoutPasswords(list []domain.User) []domain.User {
	for i := range list {
		list[i].PasswordHash = ""
	}
	return list
}
