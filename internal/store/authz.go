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
// The scope is nine columns wide as of M7, which is DESIGN.md 7's full set for
// the first time: site_id and doc_kind and state came with the identity
// migration, document_id with M3 and workflow_id with M4, and 0009 adds
// category_id, category_deep and collection_id with the tables they point at.
// domain.Scope and internal/authz have carried all nine since M2, so the
// resolver did not change as each column landed -- only this projection and
// scanGrant did, which is what "the resolver does not change when a column
// lands" was for.
//
// The category's path is joined in rather than read separately. A subtree
// match is a prefix test on it and internal/authz performs no I/O
// (DESIGN.md 7.2), so whoever loads the grant loads the path: a grant that
// arrived without one would be a constraint that silently matches nothing,
// which domain.Scope.Validate refuses for exactly that reason.
const grantColumns = `
	g.id AS id, g.role_id AS role_id, g.privilege AS privilege,
	g.site_id AS site_id, g.doc_kind AS doc_kind,
	g.category_id AS category_id, cat.path AS category_path,
	g.category_deep AS category_deep,
	g.workflow_id AS workflow_id, g.state AS state,
	g.collection_id AS collection_id, g.document_id AS document_id,
	g.created_at AS created_at, g.created_by AS created_by`

// grantFrom is the FROM clause every grant read shares. The join is LEFT
// because a category constraint is a wildcard in most grants and an unmatched
// one must not drop the row.
const grantFrom = `
	  FROM grants g
	  LEFT JOIN categories cat ON cat.id = g.category_id`

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
		SELECT `+grantColumns+grantFrom+`
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
			`SELECT `+grantColumns+grantFrom+` WHERE g.role_id = :role_id ORDER BY g.id`,
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
			INSERT INTO grants (role_id, privilege, site_id, doc_kind, category_id, category_deep,
			                    workflow_id, state, collection_id, document_id, created_at, created_by)
			VALUES (:role_id, :privilege, :site_id, :doc_kind, :category_id, :category_deep,
			        :workflow_id, :state, :collection_id, :document_id, :created_at, :created_by)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":role_id", g.RoleID)
				stmt.SetInt64(":privilege", int64(g.Privilege))
				bindNullInt64(stmt, ":site_id", g.Scope.SiteID)
				bindNullText(stmt, ":doc_kind", g.Scope.DocKind)
				bindNullInt64(stmt, ":category_id", g.Scope.CategoryID)
				stmt.SetBool(":category_deep", g.Scope.CategoryDeep)
				bindNullInt64(stmt, ":workflow_id", g.Scope.WorkflowID)
				bindNullText(stmt, ":state", g.Scope.State)
				bindNullInt64(stmt, ":collection_id", g.Scope.CollectionID)
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
			SiteID:       nullInt64(stmt, "site_id"),
			DocKind:      nullText(stmt, "doc_kind"),
			CategoryID:   nullInt64(stmt, "category_id"),
			CategoryPath: nullText(stmt, "category_path"),
			CategoryDeep: stmt.GetBool("category_deep"),
			WorkflowID:   nullInt64(stmt, "workflow_id"),
			State:        nullText(stmt, "state"),
			CollectionID: nullInt64(stmt, "collection_id"),
			DocumentID:   nullInt64(stmt, "document_id"),
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

// CreateSite inserts a site and its root category, in one transaction.
//
// The root is not optional and it is not somebody else's job. "A site has a
// root category" is what makes path arithmetic total: a document filed nowhere
// in particular is filed at "/", a URI built from a site with no root would
// have no leading slash to start from, and a category created later needs a
// parent to hang off. Migration 0009 wrote one for every site that already
// existed; this writes one for every site created afterwards, so the rule has
// no gap between the two.
//
// The root's uid is derived from the site's rather than minted, which is what
// 0009 does and for the same reason: the row is a consequence of the site
// rather than a thing somebody named, and a stable identifier lets a fixture
// and an operator name it.
func (db *DB) CreateSite(ctx context.Context, s NewSite) (int64, error) {
	var id int64
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
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
		return insertCategory(conn, domain.Category{
			UID:       "R" + s.UID,
			SiteID:    id,
			Directory: "",
			Path:      domain.RootPath,
			Name:      s.Name,
		})
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
