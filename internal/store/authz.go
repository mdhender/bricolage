// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Roles, role assignment, and grants. All SQL for authorization lives here;
// internal/authz resolves what this loads and performs no I/O (invariant 2).

// CreateRole inserts a role and returns it with its id. A duplicate slug is a
// *ConstraintError answering to domain.ErrConflict, which is how "cmsdb seed"
// is idempotent.
func (db *DB) CreateRole(ctx context.Context, slug, name string) (domain.Role, error) {
	var r domain.Role
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "creating role "+slug,
			`INSERT INTO roles (slug, name) VALUES (:slug, :name)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":slug", slug)
				stmt.SetText(":name", name)
			}, nil)
		if err != nil {
			return err
		}
		r = domain.Role{ID: conn.LastInsertRowID(), Slug: slug, Name: name}
		return nil
	})
	return r, err
}

// RoleBySlug reads a role by its external identifier.
func (db *DB) RoleBySlug(ctx context.Context, slug string) (domain.Role, error) {
	var r domain.Role
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return roleBySlug(conn, slug, &r)
	})
	return r, err
}

func roleBySlug(conn *sqlite.Conn, slug string, r *domain.Role) error {
	return one(conn, fmt.Sprintf("role %q", slug),
		`SELECT id, slug, name FROM roles WHERE slug = :slug`,
		func(stmt *sqlite.Stmt) { stmt.SetText(":slug", slug) },
		func(stmt *sqlite.Stmt) error {
			*r = domain.Role{
				ID:   stmt.GetInt64("id"),
				Slug: stmt.GetText("slug"),
				Name: stmt.GetText("name"),
			}
			return nil
		})
}

// AssignRole gives a user a role. Assigning a role twice is not an error: the
// end state is what was asked for, and "cmsdb seed" runs more than once.
func (db *DB) AssignRole(ctx context.Context, userID, roleID int64) error {
	return db.Write(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("assigning role %d to user %d", roleID, userID),
			`INSERT OR IGNORE INTO user_roles (user_id, role_id) VALUES (:user_id, :role_id)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":user_id", userID)
				stmt.SetInt64(":role_id", roleID)
			}, nil)
	})
}

// RolesForUser returns the roles a user holds, in slug order so that the list
// is stable in output and in tests.
func (db *DB) RolesForUser(ctx context.Context, userID int64) ([]domain.Role, error) {
	var roles []domain.Role
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		roles, err = rolesForUser(conn, userID)
		return err
	})
	return roles, err
}

func rolesForUser(conn *sqlite.Conn, userID int64) ([]domain.Role, error) {
	var roles []domain.Role
	err := run(conn, fmt.Sprintf("roles for user %d", userID), `
		SELECT r.id AS id, r.slug AS slug, r.name AS name
		  FROM roles r
		  JOIN user_roles ur ON ur.role_id = r.id
		 WHERE ur.user_id = :user_id
		 ORDER BY r.slug`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":user_id", userID) },
		func(stmt *sqlite.Stmt) error {
			roles = append(roles, domain.Role{
				ID:   stmt.GetInt64("id"),
				Slug: stmt.GetText("slug"),
				Name: stmt.GetText("name"),
			})
			return nil
		})
	return roles, err
}

// grantColumns is the projection every grant read shares.
//
// The scope is five columns wide today: document_id arrived with M3, which
// created the table it points at (internal/migrate/schema/0004_documents.sql).
// DESIGN.md 7 gives the scope nine dimensions; the rest arrive the same way,
// each added by the migration that creates its target table. domain.Scope and
// internal/authz carry all nine now, so the resolver does not change when a
// column lands -- only this projection and scanGrant do.
const grantColumns = `id, role_id, privilege, site_id, doc_kind, state, document_id, created_at, created_by`

// GrantsForUser returns every grant carried by every role the user holds.
//
// This is the query DESIGN.md 7.2 means by "load a user's grants once per
// request": the resolver is a pure function over what this returns, and the
// SQL equivalent of resolution is the definition of the rule rather than the
// hot path.
func (db *DB) GrantsForUser(ctx context.Context, userID int64) ([]domain.Grant, error) {
	var grants []domain.Grant
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		grants, err = grantsForUser(conn, userID)
		return err
	})
	return grants, err
}

func grantsForUser(conn *sqlite.Conn, userID int64) ([]domain.Grant, error) {
	var grants []domain.Grant
	err := run(conn, fmt.Sprintf("grants for user %d", userID), `
		SELECT g.id AS id, g.role_id AS role_id, g.privilege AS privilege,
		       g.site_id AS site_id, g.doc_kind AS doc_kind, g.state AS state,
		       g.document_id AS document_id,
		       g.created_at AS created_at, g.created_by AS created_by
		  FROM grants g
		  JOIN user_roles ur ON ur.role_id = g.role_id
		 WHERE ur.user_id = :user_id
		 ORDER BY g.id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":user_id", userID) },
		func(stmt *sqlite.Stmt) error {
			g, err := scanGrant(stmt)
			if err != nil {
				return err
			}
			grants = append(grants, g)
			return nil
		})
	return grants, err
}

// GrantsForRole returns the grants one role carries.
func (db *DB) GrantsForRole(ctx context.Context, roleID int64) ([]domain.Grant, error) {
	var grants []domain.Grant
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("grants for role %d", roleID),
			`SELECT `+grantColumns+` FROM grants WHERE role_id = :role_id ORDER BY id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":role_id", roleID) },
			func(stmt *sqlite.Stmt) error {
				g, err := scanGrant(stmt)
				if err != nil {
					return err
				}
				grants = append(grants, g)
				return nil
			})
	})
	return grants, err
}

// CreateGrant writes a grant.
//
// It performs no escalation check, deliberately: the check needs the actor's
// own grants, which is a service concern, and putting a policy decision in the
// store would give it two homes. internal/service.CreateGrant is the writing
// path invariant 12 names, and it is the only caller that takes a request.
func (db *DB) CreateGrant(ctx context.Context, g domain.Grant) (domain.Grant, error) {
	if err := g.Validate(); err != nil {
		return domain.Grant{}, err
	}
	out := g
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("granting %s over %s", g.Privilege, g.Scope), `
			INSERT INTO grants (role_id, privilege, site_id, doc_kind, state, document_id, created_at, created_by)
			VALUES (:role_id, :privilege, :site_id, :doc_kind, :state, :document_id, :created_at, :created_by)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":role_id", g.RoleID)
				stmt.SetInt64(":privilege", int64(g.Privilege))
				bindNullInt64(stmt, ":site_id", g.Scope.SiteID)
				bindNullText(stmt, ":doc_kind", g.Scope.DocKind)
				bindNullText(stmt, ":state", g.Scope.State)
				bindNullInt64(stmt, ":document_id", g.Scope.DocumentID)
				stmt.SetText(":created_at", formatTime(g.CreatedAt))
				if g.CreatedBy == 0 {
					stmt.SetNull(":created_by")
				} else {
					stmt.SetInt64(":created_by", g.CreatedBy)
				}
			}, nil)
		if err != nil {
			return err
		}
		out.ID = conn.LastInsertRowID()
		return nil
	})
	return out, err
}

func scanGrant(stmt *sqlite.Stmt) (domain.Grant, error) {
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Grant{}, fmt.Errorf("grant %d: created_at: %w", stmt.GetInt64("id"), err)
	}
	var createdBy int64
	if by := nullInt64(stmt, "created_by"); by != nil {
		createdBy = *by
	}
	return domain.Grant{
		ID:        stmt.GetInt64("id"),
		RoleID:    stmt.GetInt64("role_id"),
		Privilege: domain.Privilege(stmt.GetInt64("privilege")),
		Scope: domain.Scope{
			SiteID:     nullInt64(stmt, "site_id"),
			DocKind:    nullText(stmt, "doc_kind"),
			State:      nullText(stmt, "state"),
			DocumentID: nullInt64(stmt, "document_id"),

			// CategoryDeep is the schema's default until the column exists.
			// A grant with no category constraint is unaffected by it.
			CategoryDeep: true,
		},
		CreatedAt: created,
		CreatedBy: createdBy,
	}, nil
}

// NewSite is what CreateSite is given.
type NewSite struct {
	UID    string
	Name   string
	Domain string
}

// CreateSite inserts a site. It is here rather than in a file of its own
// because M2 needs exactly one of them -- the one "cmsdb seed" creates -- and
// because grants.site_id points at it. The rest of DESIGN.md 5.3 arrives with
// the milestone that has documents to put in a category.
func (db *DB) CreateSite(ctx context.Context, s NewSite) (int64, error) {
	var id int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "creating site "+s.Name, `
			INSERT INTO sites (uid, name, domain, active) VALUES (:uid, :name, :domain, 1)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", s.UID)
				stmt.SetText(":name", s.Name)
				stmt.SetText(":domain", s.Domain)
			}, nil)
		if err != nil {
			return err
		}
		id = conn.LastInsertRowID()
		return nil
	})
	return id, err
}

// SiteByDomain reads a site by the domain it serves, which is what "cmsdb
// seed" checks before creating one.
func (db *DB) SiteByDomain(ctx context.Context, domainName string) (int64, error) {
	var id int64
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("site %q", domainName),
			`SELECT id FROM sites WHERE domain = :domain`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":domain", domainName) },
			func(stmt *sqlite.Stmt) error {
				id = stmt.GetInt64("id")
				return nil
			})
	})
	return id, err
}

// Identity assembles who a user is and what they may do, in one read: the
// user, the roles they hold, and the grants those roles carry.
//
// One call and one connection, so the three reads cannot see three different
// states of the database. This is what the authentication middleware puts on
// the request context, and what "no elevation" means in PLAN.md M2
// acceptance 10 -- there is one way to build it, so a session issued by the
// development route carries exactly what a password login carries.
func (db *DB) Identity(ctx context.Context, userID int64) (domain.Identity, error) {
	var id domain.Identity
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		if err := userByID(conn, userID, &id.User); err != nil {
			return err
		}
		var err error
		if id.Roles, err = rolesForUser(conn, userID); err != nil {
			return err
		}
		id.Grants, err = grantsForUser(conn, userID)
		return err
	})
	return id, err
}
