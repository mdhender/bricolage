// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// sessionColumns is the projection every session read shares.
const sessionColumns = `id, user_id, token_sha256, created_at, expires_at, last_seen_at`

// NewSession is what CreateSession is given. The token hash is computed by the
// caller, which is also the only place that ever holds the token itself
// (internal/authz.NewToken).
type NewSession struct {
	UserID    int64
	TokenHash string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession writes a session row.
//
// It stores the SHA-256 of the token and never the token: this table is not a
// set of usable credentials even to somebody holding a copy of the file.
func (db *DB) CreateSession(ctx context.Context, s NewSession) (domain.Session, error) {
	out := domain.Session{
		UserID:     s.UserID,
		TokenHash:  s.TokenHash,
		CreatedAt:  s.CreatedAt.UTC(),
		ExpiresAt:  s.ExpiresAt.UTC(),
		LastSeenAt: s.CreatedAt.UTC(),
	}
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("creating a session for user %d", s.UserID), `
			INSERT INTO sessions (user_id, token_sha256, created_at, expires_at, last_seen_at)
			VALUES (:user_id, :token_sha256, :created_at, :expires_at, :last_seen_at)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":user_id", s.UserID)
				stmt.SetText(":token_sha256", s.TokenHash)
				stmt.SetText(":created_at", formatTime(s.CreatedAt))
				stmt.SetText(":expires_at", formatTime(s.ExpiresAt))
				stmt.SetText(":last_seen_at", formatTime(s.CreatedAt))
			}, nil)
		if err != nil {
			return err
		}
		out.ID = conn.LastInsertRowID()
		return nil
	})
	return out, err
}

// SessionByTokenHash reads a session by the hash of its token.
//
// It does not judge expiry. Reading a session and deciding whether it is still
// good are different questions, and the caller with the clock answers the
// second one (invariant 3).
func (db *DB) SessionByTokenHash(ctx context.Context, hash string) (domain.Session, error) {
	var s domain.Session
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, "session",
			`SELECT `+sessionColumns+` FROM sessions WHERE token_sha256 = :hash`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":hash", hash) },
			func(stmt *sqlite.Stmt) error {
				var err error
				s, err = scanSession(stmt)
				return err
			})
	})
	return s, err
}

// TouchSession records that a session was used at now.
//
// It is a write on the one write connection, so it is not done on every
// request: the caller compares last_seen_at against a threshold first. What
// this buys is the ability to answer "when was this token last used", which is
// the question asked about a token somebody thinks was stolen.
func (db *DB) TouchSession(ctx context.Context, id int64, now time.Time) error {
	return db.Write(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("touching session %d", id),
			`UPDATE sessions SET last_seen_at = :now WHERE id = :id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":now", formatTime(now))
				stmt.SetInt64(":id", id)
			}, nil)
	})
}

// DeleteSession removes one session, which is what logging out is.
func (db *DB) DeleteSession(ctx context.Context, id int64) error {
	return db.Write(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("deleting session %d", id),
			`DELETE FROM sessions WHERE id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) }, nil)
	})
}

// DeleteExpiredSessions removes every session that expired before now, and
// reports how many. Nothing calls it on a schedule yet; "cmsdb check" and the
// job worker are where it belongs when there is one.
func (db *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "deleting expired sessions",
			`DELETE FROM sessions WHERE expires_at <= :now`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":now", formatTime(now)) }, nil)
		if err != nil {
			return err
		}
		n = conn.Changes()
		return nil
	})
	return n, err
}

func scanSession(stmt *sqlite.Stmt) (domain.Session, error) {
	id := stmt.GetInt64("id")
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Session{}, fmt.Errorf("session %d: created_at: %w", id, err)
	}
	expires, err := parseTime(stmt.GetText("expires_at"))
	if err != nil {
		return domain.Session{}, fmt.Errorf("session %d: expires_at: %w", id, err)
	}
	seen, err := parseTime(stmt.GetText("last_seen_at"))
	if err != nil {
		return domain.Session{}, fmt.Errorf("session %d: last_seen_at: %w", id, err)
	}
	return domain.Session{
		ID:         id,
		UserID:     stmt.GetInt64("user_id"),
		TokenHash:  stmt.GetText("token_sha256"),
		CreatedAt:  created,
		ExpiresAt:  expires,
		LastSeenAt: seen,
	}, nil
}
