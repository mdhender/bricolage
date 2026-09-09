// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Categories, the filing of documents into them, and output channels. All the
// SQL for M7 is here (invariant 2).
//
// The statement worth reading is in moveSubtree. Moving a category has to
// rewrite the materialised path of every descendant, and PLAN.md M7
// acceptance 1 says it is one statement -- not because a loop would be slow
// but because a loop can stop half way, and half a rewritten subtree is a set
// of documents whose grants and URIs disagree with their parents'.

// categoryColumns is the projection every category read shares.
const categoryColumns = `id, uid, site_id, parent_id, directory, path, name`

// NewCategory is what CreateCategory is given.
//
// The path is not here: it is computed inside the transaction from the
// parent's, by domain.JoinPath, so that the arithmetic is written down once
// and so that a parent moved by somebody else between the read and the write
// cannot leave the child hanging off a path that no longer exists.
type NewCategory struct {
	UID string

	// ParentID is the category this one goes under. It is required: a site's
	// root is created with its site and nothing else creates one.
	ParentID int64

	Directory string
	Name      string

	// Event is recorded in the same transaction as the insert. Its SubjectID
	// is filled in here, because the category has no id until the INSERT has
	// run.
	Event domain.Event
}

// CreateCategory writes a category under its parent and records the event.
func (db *DB) CreateCategory(ctx context.Context, n NewCategory) (domain.Category, error) {
	var out domain.Category
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		var parent domain.Category
		if err := categoryByID(conn, n.ParentID, &parent); err != nil {
			return err
		}
		path, err := domain.JoinPath(parent.Path, n.Directory)
		if err != nil {
			return err
		}

		if err := insertCategory(conn, domain.Category{
			UID:       n.UID,
			SiteID:    parent.SiteID,
			ParentID:  parent.ID,
			Directory: n.Directory,
			Path:      path,
			Name:      n.Name,
		}); err != nil {
			return err
		}
		id := conn.LastInsertRowID()

		n.Event.SubjectKind = domain.SubjectCategory
		n.Event.SubjectID = id
		if _, err := recordEvent(conn, n.Event); err != nil {
			return err
		}
		return categoryByID(conn, id, &out)
	})
	return out, err
}

// insertCategory writes one row on a connection the caller holds. It is shared
// with CreateSite, which writes a site's root in the same transaction as the
// site.
func insertCategory(conn *sqlite.Conn, c domain.Category) error {
	return run(conn, fmt.Sprintf("creating category %q", c.Path), `
		INSERT INTO categories (uid, site_id, parent_id, directory, path, name)
		VALUES (:uid, :site_id, :parent_id, :directory, :path, :name)`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":uid", c.UID)
			stmt.SetInt64(":site_id", c.SiteID)
			if c.ParentID == 0 {
				stmt.SetNull(":parent_id")
			} else {
				stmt.SetInt64(":parent_id", c.ParentID)
			}
			stmt.SetText(":directory", c.Directory)
			stmt.SetText(":path", c.Path)
			stmt.SetText(":name", c.Name)
		}, nil)
}

// CategoryByUID reads a category by the identifier the API speaks
// (invariant 10).
func (db *DB) CategoryByUID(ctx context.Context, uid string) (domain.Category, error) {
	var c domain.Category
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return categoryByUID(conn, uid, &c)
	})
	return c, err
}

func categoryByUID(conn *sqlite.Conn, uid string, c *domain.Category) error {
	return one(conn, fmt.Sprintf("category %q", uid),
		`SELECT `+categoryColumns+` FROM categories WHERE uid = :uid`,
		func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
		func(stmt *sqlite.Stmt) error { *c = scanCategory(stmt); return nil })
}

func categoryByID(conn *sqlite.Conn, id int64, c *domain.Category) error {
	return one(conn, fmt.Sprintf("category %d", id),
		`SELECT `+categoryColumns+` FROM categories WHERE id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error { *c = scanCategory(stmt); return nil })
}

// CategoryByPath reads a category by its materialised path on one site, which
// is how a client names one it has not seen the uid of: "/features/film/".
func (db *DB) CategoryByPath(ctx context.Context, siteID int64, path string) (domain.Category, error) {
	var c domain.Category
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return categoryByPath(conn, siteID, path, &c)
	})
	return c, err
}

func categoryByPath(conn *sqlite.Conn, siteID int64, path string, c *domain.Category) error {
	return one(conn, fmt.Sprintf("category %q on site %d", path, siteID),
		`SELECT `+categoryColumns+` FROM categories WHERE site_id = :site_id AND path = :path`,
		func(stmt *sqlite.Stmt) {
			stmt.SetInt64(":site_id", siteID)
			stmt.SetText(":path", path)
		},
		func(stmt *sqlite.Stmt) error { *c = scanCategory(stmt); return nil })
}

// ListCategories returns one site's categories in path order, which is
// depth-first order: a listing that reads as a tree without the caller sorting
// it, because the materialised path sorts that way already.
func (db *DB) ListCategories(ctx context.Context, siteID int64) ([]domain.Category, error) {
	var out []domain.Category
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("categories of site %d", siteID),
			`SELECT `+categoryColumns+` FROM categories WHERE site_id = :site_id ORDER BY path`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":site_id", siteID) },
			func(stmt *sqlite.Stmt) error {
				out = append(out, scanCategory(stmt))
				return nil
			})
	})
	return out, err
}

// CategorySubtree returns a category and everything below it, in path order.
//
// The prefix test is SUBSTR rather than LIKE for the reason moveSubtree's is:
// a path may contain '%' or '_' -- both are unreserved enough for a directory
// name -- and LIKE would read them as wildcards.
func (db *DB) CategorySubtree(ctx context.Context, siteID int64, path string) ([]domain.Category, error) {
	var out []domain.Category
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("the subtree at %q on site %d", path, siteID), `
			SELECT `+categoryColumns+`
			  FROM categories
			 WHERE site_id = :site_id AND SUBSTR(path, 1, LENGTH(:path)) = :path
			 ORDER BY path`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":site_id", siteID)
				stmt.SetText(":path", path)
			},
			func(stmt *sqlite.Stmt) error {
				out = append(out, scanCategory(stmt))
				return nil
			})
	})
	return out, err
}

func scanCategory(stmt *sqlite.Stmt) domain.Category {
	c := domain.Category{
		ID:        stmt.GetInt64("id"),
		UID:       stmt.GetText("uid"),
		SiteID:    stmt.GetInt64("site_id"),
		Directory: stmt.GetText("directory"),
		Path:      stmt.GetText("path"),
		Name:      stmt.GetText("name"),
	}
	if v := nullInt64(stmt, "parent_id"); v != nil {
		c.ParentID = *v
	}
	return c
}

// MoveCategory moves or renames a category and rewrites the path of every
// descendant (PLAN.md M7 acceptance 1).
//
// The two refusals -- a root cannot move, and nothing moves into its own
// subtree -- are domain.CategoryMove.Resolve's, which is where they belong:
// they are statements about paths and this package decides nothing.
func (db *DB) MoveCategory(ctx context.Context, id int64, move domain.CategoryMove, event domain.Event) (domain.Category, error) {
	var out domain.Category
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		var c domain.Category
		if err := categoryByID(conn, id, &c); err != nil {
			return err
		}

		parent := domain.Category{}
		if move.ParentPath != nil {
			if err := categoryByPath(conn, c.SiteID, *move.ParentPath, &parent); err != nil {
				return err
			}
		} else if err := categoryByID(conn, c.ParentID, &parent); err != nil {
			return err
		}

		newPath, err := move.Resolve(c, parent)
		if err != nil {
			return err
		}
		if newPath == c.Path {
			return fmt.Errorf("category %q is already there: %w", c.Path, domain.ErrConflict)
		}

		directory := c.Directory
		if move.Directory != nil {
			directory = *move.Directory
		}
		err = run(conn, fmt.Sprintf("moving category %q to %q", c.Path, newPath), `
			UPDATE categories SET parent_id = :parent_id, directory = :directory WHERE id = :id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":parent_id", parent.ID)
				stmt.SetText(":directory", directory)
				stmt.SetInt64(":id", c.ID)
			}, nil)
		if err != nil {
			return err
		}
		if err := moveSubtree(conn, c.SiteID, c.Path, newPath); err != nil {
			return err
		}

		event.SubjectKind = domain.SubjectCategory
		event.SubjectID = c.ID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return categoryByID(conn, c.ID, &out)
	})
	return out, err
}

// moveSubtree rewrites the path of the moved category and of every descendant,
// in one statement (PLAN.md M7 acceptance 1).
//
// One statement is the requirement and the reason is atomicity of shape rather
// than speed: a loop that stopped half way would leave a subtree whose rows
// disagree about where they are, and both things that read this column --
// URI construction and category-scoped grants -- would then be right about
// some rows and wrong about others, with nothing to notice.
//
// The prefix test is SUBSTR and not LIKE because a directory name may contain
// '%' or '_', which LIKE would read as wildcards: "/50%_off/" would match half
// the site. Detecting that by escaping the pattern is a thing to get right
// every time this query is written; comparing a fixed-length prefix is a thing
// that cannot be got wrong.
//
// The rewrite is ordered by descending path length so that the deepest rows
// move first. UNIQUE (site_id, path) is checked per row as SQLite updates it,
// and a rename that swaps two siblings -- /a/ to /b/ while /b/ still exists --
// would otherwise collide on a row that is about to move out of the way. It
// does not save every such move, and it is not meant to: a genuine collision
// is a *ConstraintError answering to domain.ErrConflict, which is the right
// answer to "there is already a category there".
func moveSubtree(conn *sqlite.Conn, siteID int64, oldPrefix, newPrefix string) error {
	return run(conn, fmt.Sprintf("rewriting the subtree at %q to %q", oldPrefix, newPrefix), `
		UPDATE categories
		   SET path = :new_prefix || SUBSTR(path, LENGTH(:old_prefix) + 1)
		 WHERE id IN (
		       SELECT id FROM categories
		        WHERE site_id = :site_id
		          AND SUBSTR(path, 1, LENGTH(:old_prefix)) = :old_prefix
		        ORDER BY LENGTH(path) DESC)`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":new_prefix", newPrefix)
			stmt.SetText(":old_prefix", oldPrefix)
			stmt.SetInt64(":site_id", siteID)
		}, nil)
}

// DeleteCategory removes a category that nothing depends on.
//
// A category with children or with documents filed in it is refused rather
// than cascaded. Cascading would silently unfile documents -- their URIs would
// change and their category-scoped grants would stop matching -- and the
// person who deleted a section would find out from a reader.
func (db *DB) DeleteCategory(ctx context.Context, id int64, event domain.Event) error {
	return db.Tx(ctx, func(conn *sqlite.Conn) error {
		var c domain.Category
		if err := categoryByID(conn, id, &c); err != nil {
			return err
		}
		if c.ParentID == 0 {
			return fmt.Errorf("category %q is site %d's root and cannot be deleted: %w",
				c.Path, c.SiteID, domain.ErrConflict)
		}

		children, err := countRows(conn, "children of category "+c.Path,
			`SELECT COUNT(*) AS n FROM categories WHERE parent_id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) })
		if err != nil {
			return err
		}
		if children > 0 {
			return fmt.Errorf("category %q has %d categories under it: %w", c.Path, children, domain.ErrConflict)
		}

		filed, err := countRows(conn, "documents filed in category "+c.Path,
			`SELECT COUNT(*) AS n FROM document_categories WHERE category_id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) })
		if err != nil {
			return err
		}
		if filed > 0 {
			return fmt.Errorf("category %q has %d documents filed in it: %w", c.Path, filed, domain.ErrConflict)
		}

		// The event is written before the row goes, against the id it names,
		// so that a deleted category still has a history (invariant 7).
		event.SubjectKind = domain.SubjectCategory
		event.SubjectID = c.ID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return run(conn, "deleting category "+c.Path,
			`DELETE FROM categories WHERE id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) }, nil)
	})
}

// countRows runs a COUNT(*) aliased "n".
func countRows(conn *sqlite.Conn, what, query string, bind func(*sqlite.Stmt)) (int64, error) {
	var n int64
	err := one(conn, what, query, bind, func(stmt *sqlite.Stmt) error {
		n = stmt.GetInt64("n")
		return nil
	})
	return n, err
}

// CategoriesForDocument returns where a document is filed, primary first.
func (db *DB) CategoriesForDocument(ctx context.Context, documentID int64) ([]domain.Filing, error) {
	var out []domain.Filing
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		out, err = categoriesForDocument(conn, documentID)
		return err
	})
	return out, err
}

func categoriesForDocument(conn *sqlite.Conn, documentID int64) ([]domain.Filing, error) {
	var out []domain.Filing
	err := run(conn, fmt.Sprintf("categories of document %d", documentID), `
		SELECT c.id AS id, c.uid AS uid, c.site_id AS site_id, c.parent_id AS parent_id,
		       c.directory AS directory, c.path AS path, c.name AS name,
		       dc.primary_cat AS primary_cat
		  FROM document_categories dc
		  JOIN categories c ON c.id = dc.category_id
		 WHERE dc.document_id = :document_id
		 ORDER BY dc.primary_cat DESC, c.path`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
		func(stmt *sqlite.Stmt) error {
			out = append(out, domain.Filing{
				Category: scanCategory(stmt),
				Primary:  stmt.GetBool("primary_cat"),
			})
			return nil
		})
	return out, err
}

// SetDocumentCategories replaces where a document is filed, in one
// transaction.
//
// Replace rather than add: "these are the categories" is the request a client
// makes, and computing the difference here means the client does not have to
// send two requests that could half succeed. The primary is the first of them,
// which is what the partial unique index makes single.
func (db *DB) SetDocumentCategories(ctx context.Context, documentID int64, categoryIDs []int64, now time.Time, event domain.Event) ([]domain.Filing, error) {
	var out []domain.Filing
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("clearing the categories of document %d", documentID),
			`DELETE FROM document_categories WHERE document_id = :document_id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) }, nil)
		if err != nil {
			return err
		}
		for i, categoryID := range categoryIDs {
			err := run(conn, fmt.Sprintf("filing document %d in category %d", documentID, categoryID), `
				INSERT INTO document_categories (document_id, category_id, primary_cat)
				VALUES (:document_id, :category_id, :primary_cat)`,
				func(stmt *sqlite.Stmt) {
					stmt.SetInt64(":document_id", documentID)
					stmt.SetInt64(":category_id", categoryID)
					stmt.SetBool(":primary_cat", i == 0)
				}, nil)
			if err != nil {
				return err
			}
		}
		if err := touchDocument(conn, documentID, now); err != nil {
			return err
		}

		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = documentID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		out, err = categoriesForDocument(conn, documentID)
		return err
	})
	return out, err
}

// touchDocument moves updated_at without touching anything else. Filing a
// document is a change to the document row, and a row whose updated_at did not
// move is a row a cache will not refresh.
func touchDocument(conn *sqlite.Conn, documentID int64, now time.Time) error {
	return run(conn, fmt.Sprintf("touching document %d", documentID),
		`UPDATE documents SET updated_at = :now WHERE id = :id`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":now", formatTime(now))
			stmt.SetInt64(":id", documentID)
		}, nil)
}
