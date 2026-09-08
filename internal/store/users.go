// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// userColumns is the projection every user read shares, so that scanUser has
// one shape to read and adding a column is one edit.
const userColumns = `id, uid, email, name, active, created_at, password_hash`

// NewUser is what CreateUser is given. The caller supplies the uid and the
// timestamp, because minting an identifier needs a clock and the clock belongs
// to the caller (invariant 3).
type NewUser struct {
	UID          string
	Email        string
	Name         string
	PasswordHash string
	CreatedAt    time.Time
}

// CreateUser inserts a user and returns it with its assigned id.
//
// The email is stored as given: normalising it is the caller's decision and
// domain.NormalizeEmail is where it happens, so that a lookup and an insert
// cannot disagree about what folding means.
//
// A duplicate email is a *ConstraintError answering to domain.ErrConflict,
// classified by result code and never by message text (invariant 11). That is
// what makes "cmsdb bootstrap admin" idempotent without a read-then-write race.
func (db *DB) CreateUser(ctx context.Context, u NewUser) (domain.User, error) {
	var out domain.User
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "creating user "+u.Email, `
			INSERT INTO users (uid, email, name, active, created_at, password_hash)
			VALUES (:uid, :email, :name, 1, :created_at, :password_hash)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", u.UID)
				stmt.SetText(":email", u.Email)
				stmt.SetText(":name", u.Name)
				stmt.SetText(":created_at", formatTime(u.CreatedAt))
				stmt.SetText(":password_hash", u.PasswordHash)
			}, nil)
		if err != nil {
			return err
		}
		out = domain.User{
			ID:           conn.LastInsertRowID(),
			UID:          u.UID,
			Email:        u.Email,
			Name:         u.Name,
			Active:       true,
			CreatedAt:    u.CreatedAt.UTC(),
			PasswordHash: u.PasswordHash,
		}
		return nil
	})
	return out, err
}

// UserByEmail reads a user by email address. The address is matched exactly;
// fold it with domain.NormalizeEmail first.
func (db *DB) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	var u domain.User
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("user %q", email),
			`SELECT `+userColumns+` FROM users WHERE email = :email`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":email", email) },
			func(stmt *sqlite.Stmt) error {
				var err error
				u, err = scanUser(stmt)
				return err
			})
	})
	return u, err
}

// UserByUID reads a user by the identifier the API speaks (invariant 10).
func (db *DB) UserByUID(ctx context.Context, uid string) (domain.User, error) {
	var u domain.User
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("user %q", uid),
			`SELECT `+userColumns+` FROM users WHERE uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error {
				var err error
				u, err = scanUser(stmt)
				return err
			})
	})
	return u, err
}

// UserByID reads a user by the internal key. It exists for the paths that
// already hold one -- a session row, an event's actor -- and never for
// anything that came off the wire (invariant 10).
func (db *DB) UserByID(ctx context.Context, id int64) (domain.User, error) {
	var u domain.User
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return userByID(conn, id, &u)
	})
	return u, err
}

func userByID(conn *sqlite.Conn, id int64, u *domain.User) error {
	return one(conn, fmt.Sprintf("user %d", id),
		`SELECT `+userColumns+` FROM users WHERE id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*u, err = scanUser(stmt)
			return err
		})
}

// CountUsers reports how many users exist, which is what "cmsdb bootstrap
// admin" asks to decide whether it is bootstrapping anything.
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, "counting users", `SELECT COUNT(*) AS n FROM users`, nil,
			func(stmt *sqlite.Stmt) error {
				n = int(stmt.GetInt64("n"))
				return nil
			})
	})
	return n, err
}

func scanUser(stmt *sqlite.Stmt) (domain.User, error) {
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.User{}, fmt.Errorf("user %d: created_at: %w", stmt.GetInt64("id"), err)
	}
	return domain.User{
		ID:           stmt.GetInt64("id"),
		UID:          stmt.GetText("uid"),
		Email:        stmt.GetText("email"),
		Name:         stmt.GetText("name"),
		Active:       stmt.GetBool("active"),
		CreatedAt:    created,
		PasswordHash: stmt.GetText("password_hash"),
	}, nil
}
