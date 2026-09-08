// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// LoginResult is a new session and the token that reaches it. The token is
// returned once and never stored: the database holds only its SHA-256.
type LoginResult struct {
	Token    string
	Session  domain.Session
	Identity domain.Identity
}

// Login authenticates an email and password and issues a session
// (DESIGN.md 12, POST /api/v1/sessions).
//
// Every failure returns authz.ErrBadCredentials, which wraps
// domain.ErrUnauthenticated. Four causes, one answer: an unknown address, an
// inactive user, a user with no password, and the wrong password are
// indistinguishable from outside. Distinguishing them tells an attacker which
// addresses have accounts.
//
// clientAddr is the address the trusted-proxy middleware resolved, and it is
// passed in rather than looked up because this layer has no request
// (invariant 14, DESIGN.md 11). It goes in the event, which is the point of
// resolving it: "where was this session created from" is the question asked
// about a session somebody thinks was stolen, and the answer has to be the
// resolved address rather than whatever X-Forwarded-For said.
func (s *Service) Login(ctx context.Context, email, password, clientAddr string) (LoginResult, error) {
	email = domain.NormalizeEmail(email)

	u, err := s.db.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The bcrypt comparison is skipped, so this path is faster than a
			// real login. That is a timing signal, and it is one we accept:
			// closing it means hashing against a dummy on every miss, and the
			// address enumeration it would prevent is available from the
			// password reset flow of every system that has one.
			return LoginResult{}, authz.ErrBadCredentials
		}
		return LoginResult{}, err
	}
	if err := authz.VerifyPassword(u, password); err != nil {
		return LoginResult{}, err
	}

	return s.issue(ctx, u, events.SessionCreated, map[string]any{
		"email":  u.Email,
		"client": clientAddr,
	})
}

// DevLogin issues a session for an existing user without a password, for
// GET /__development/log-me-in/{email} (DESIGN.md 11).
//
// Two things about it are load-bearing and neither is negotiable:
//
//   - It does not create accounts. An unknown email is domain.ErrNotFound,
//     which the route turns into a 404. This keeps the blast radius to
//     accounts that already exist, and it matches what an agent needs:
//     "cmsdb bootstrap admin", then log in as that admin.
//   - The session it issues is an ordinary session. It goes through the same
//     issue as a password login, gets the ordinary expiry, and grants no extra
//     privilege: the user's roles and grants apply exactly as they would after
//     a real login (PLAN.md M2 acceptance 10).
//
// The event it writes carries the email and the peer address, because that is
// the line an operator wants during an incident (acceptance 13).
func (s *Service) DevLogin(ctx context.Context, email, peer string) (LoginResult, error) {
	email = domain.NormalizeEmail(email)

	u, err := s.db.UserByEmail(ctx, email)
	if err != nil {
		return LoginResult{}, err
	}
	if !u.Active {
		return LoginResult{}, fmt.Errorf("user %q is not active: %w", email, domain.ErrNotFound)
	}

	return s.issue(ctx, u, events.SessionDevLogin, map[string]any{
		"email": u.Email,
		"peer":  peer,
	})
}

// issue mints a token, writes the session, records the event, and assembles
// the identity.
//
// Both login paths go through it, which is what makes "no elevation" a
// property of the code rather than of a review: there is one place a session
// comes from, so a development login cannot carry anything a password login
// does not.
func (s *Service) issue(ctx context.Context, u domain.User, eventType string, payload map[string]any) (LoginResult, error) {
	now := s.Now()

	token, hash, err := authz.NewToken()
	if err != nil {
		return LoginResult{}, err
	}

	sess, err := s.db.CreateSession(ctx, store.NewSession{
		UserID:    u.ID,
		TokenHash: hash,
		CreatedAt: now,
		ExpiresAt: now.Add(s.sessionTTL),
	})
	if err != nil {
		return LoginResult{}, err
	}

	if _, err := s.db.RecordEvent(ctx, domain.Event{
		Type:        eventType,
		ActorID:     u.ID,
		SubjectKind: domain.SubjectSession,
		SubjectID:   sess.ID,
		Payload:     payload,
		OccurredAt:  now,
	}); err != nil {
		return LoginResult{}, err
	}

	identity, err := s.db.Identity(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	identity.Session = sess

	return LoginResult{Token: token, Session: sess, Identity: identity}, nil
}

// Authenticate resolves a bearer token or session cookie to an identity.
//
// An unknown token and an expired session are the same answer:
// domain.ErrUnauthenticated, which the edge maps to 401 (PLAN.md M2
// acceptance 8). An expired row is deleted on the way past, so a token that
// stopped working also stops occupying the table.
func (s *Service) Authenticate(ctx context.Context, token string) (domain.Identity, error) {
	if token == "" {
		return domain.Identity{}, fmt.Errorf("no credential: %w", domain.ErrUnauthenticated)
	}

	sess, err := s.db.SessionByTokenHash(ctx, authz.HashToken(token))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Identity{}, fmt.Errorf("no such session: %w", domain.ErrUnauthenticated)
		}
		return domain.Identity{}, err
	}

	now := s.Now()
	if sess.Expired(now) {
		if err := s.db.DeleteSession(ctx, sess.ID); err != nil {
			s.log.Warn("could not delete an expired session", "session", sess.ID, "error", err)
		}
		return domain.Identity{}, fmt.Errorf("session expired at %s: %w",
			sess.ExpiresAt.Format("2006-01-02T15:04:05Z"), domain.ErrUnauthenticated)
	}

	identity, err := s.db.Identity(ctx, sess.UserID)
	if err != nil {
		return domain.Identity{}, err
	}
	if !identity.User.Active {
		return domain.Identity{}, fmt.Errorf("user is not active: %w", domain.ErrUnauthenticated)
	}
	identity.Session = sess

	// Recording use is worth a write, but not one per request: the write
	// connection is serialized, and putting a write in front of every read
	// makes the whole server as slow as its slowest writer.
	if now.Sub(sess.LastSeenAt) >= s.touchAfter {
		if err := s.db.TouchSession(ctx, sess.ID, now); err != nil {
			s.log.Warn("could not record session use", "session", sess.ID, "error", err)
		} else {
			identity.Session.LastSeenAt = now
		}
	}
	return identity, nil
}

// Logout deletes a session and records that it ended.
func (s *Service) Logout(ctx context.Context, identity domain.Identity) error {
	if identity.Session.ID == 0 {
		return fmt.Errorf("no session: %w", domain.ErrUnauthenticated)
	}
	if err := s.db.DeleteSession(ctx, identity.Session.ID); err != nil {
		return err
	}
	_, err := s.db.RecordEvent(ctx, domain.Event{
		Type:        events.SessionEnded,
		ActorID:     identity.User.ID,
		SubjectKind: domain.SubjectSession,
		SubjectID:   identity.Session.ID,
		Payload:     map[string]any{"email": identity.User.Email},
		OccurredAt:  s.Now(),
	})
	return err
}

// NewUser is what CreateUser is given. The password is hashed here and the
// clear text never reaches the store.
type NewUser struct {
	Email    string
	Name     string
	Password string
}

// CreateUser creates a user and records it.
//
// It is the path "cmsdb bootstrap admin" takes. A duplicate email comes back
// as a constraint violation answering to domain.ErrConflict, detected by
// result code (invariant 11), which is what makes bootstrap idempotent without
// a read-then-write race.
func (s *Service) CreateUser(ctx context.Context, in NewUser) (domain.User, error) {
	email := domain.NormalizeEmail(in.Email)
	if err := domain.ValidateEmail(email); err != nil {
		return domain.User{}, err
	}
	if in.Name == "" {
		return domain.User{}, fmt.Errorf("name: required: %w", domain.ErrInvalid)
	}

	hash, err := authz.HashPassword(in.Password)
	if err != nil {
		return domain.User{}, err
	}

	now := s.Now()
	uid, err := ids.New(now)
	if err != nil {
		return domain.User{}, err
	}

	u, err := s.db.CreateUser(ctx, store.NewUser{
		UID:          uid,
		Email:        email,
		Name:         in.Name,
		PasswordHash: hash,
		CreatedAt:    now,
	})
	if err != nil {
		return domain.User{}, err
	}

	if _, err := s.db.RecordEvent(ctx, domain.Event{
		Type:        events.UserCreated,
		SubjectKind: domain.SubjectUser,
		SubjectID:   u.ID,
		Payload:     map[string]any{"email": u.Email, "name": u.Name, "uid": u.UID},
		OccurredAt:  now,
	}); err != nil {
		return domain.User{}, err
	}
	return u, nil
}
