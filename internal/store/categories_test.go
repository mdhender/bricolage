// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// The store half of M7, against a real in-memory database with every migration
// applied through the same code path cmsdb uses and with foreign keys on
// (DESIGN.md 15, invariant 22).

// catFixture is a site and the category tree PLAN.md M7 acceptance 1 asks for:
// three levels and two siblings.
//
//	/
//	├── features/          (moved)
//	│   ├── film/
//	│   │   ├── reviews/
//	│   │   └── interviews/
//	│   └── books/
//	└── culture/           (the destination)
type catFixture struct {
	*docFixture
	root domain.Category
	cats map[string]domain.Category
}

func newCatFixture(t *testing.T) *catFixture {
	t.Helper()
	f := &catFixture{docFixture: newDocFixture(t), cats: map[string]domain.Category{}}

	root, err := f.db.CategoryByPath(t.Context(), f.siteID, domain.RootPath)
	if err != nil {
		t.Fatalf("every site gets a root category when it is created: %v", err)
	}
	f.root = root
	f.cats[root.Path] = root

	for _, spec := range []struct{ parent, directory string }{
		{"/", "features"},
		{"/", "culture"},
		{"/features/", "film"},
		{"/features/", "books"},
		{"/features/film/", "reviews"},
		{"/features/film/", "interviews"},
	} {
		f.add(t, spec.parent, spec.directory)
	}
	return f
}

func (f *catFixture) add(t *testing.T, parentPath, directory string) domain.Category {
	t.Helper()
	parent, ok := f.cats[parentPath]
	if !ok {
		t.Fatalf("no category at %q", parentPath)
	}
	c, err := f.db.CreateCategory(t.Context(), NewCategory{
		UID:       ids.MustNew(f.now),
		ParentID:  parent.ID,
		Directory: directory,
		Name:      directory,
		Event:     f.event(f.author.ID, events.CategoryCreated),
	})
	if err != nil {
		t.Fatalf("CreateCategory(%q, %q): %v", parentPath, directory, err)
	}
	f.cats[c.Path] = c
	return c
}

// paths reads every category on the site, as a map from uid to path, so that a
// test can check a row's new path against the row it was.
func (f *catFixture) paths(t *testing.T) map[string]string {
	t.Helper()
	all, err := f.db.ListCategories(t.Context(), f.siteID)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	out := make(map[string]string, len(all))
	for _, c := range all {
		out[c.UID] = c.Path
	}
	return out
}

// TestEverySiteHasARootCategory is what makes path arithmetic total: a
// document filed nowhere in particular is filed at "/", and a category created
// later has a parent to hang off.
func TestEverySiteHasARootCategory(t *testing.T) {
	f := newDocFixture(t)

	root, err := f.db.CategoryByPath(t.Context(), f.siteID, domain.RootPath)
	if err != nil {
		t.Fatalf("the site created by CreateSite has no root category: %v", err)
	}
	if root.Path != "/" || root.Directory != "" || root.ParentID != 0 {
		t.Errorf("the root is %+v; want path \"/\", no directory, and no parent", root)
	}
	if !root.IsRoot() {
		t.Error("the root does not report itself as one")
	}

	// One root per site, enforced by UNIQUE (site_id, path) rather than by
	// care: a second row claiming "/" is a second answer to "where does this
	// site's tree start".
	err = f.db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return insertCategory(conn, domain.Category{
			UID: ids.MustNew(f.now), SiteID: f.siteID, Path: "/", Name: "Second",
		})
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a second root was accepted: %v", err)
	}
}

// TestMoveCategoryRewritesEveryDescendant is PLAN.md M7 acceptance 1: three
// levels and two siblings, and every row verified.
func TestMoveCategoryRewritesEveryDescendant(t *testing.T) {
	f := newCatFixture(t)
	before := f.paths(t)

	moved, err := f.db.MoveCategory(t.Context(), f.cats["/features/"].ID,
		domain.CategoryMove{ParentPath: domain.Ref("/culture/")},
		f.event(f.author.ID, events.CategoryMoved))
	if err != nil {
		t.Fatalf("MoveCategory: %v", err)
	}
	if moved.Path != "/culture/features/" {
		t.Errorf("the moved category is at %q, want %q", moved.Path, "/culture/features/")
	}
	if moved.ParentID != f.cats["/culture/"].ID {
		t.Errorf("the moved category's parent is %d, want %d", moved.ParentID, f.cats["/culture/"].ID)
	}

	// Every row, checked against what domain.RewritePath says it should be.
	// The Go statement of the rule and the single SQL statement that performs
	// it are two implementations, and this is what keeps them agreeing.
	after := f.paths(t)
	if len(after) != len(before) {
		t.Fatalf("the move changed the number of categories: %d, was %d", len(after), len(before))
	}
	for uid, was := range before {
		want := domain.RewritePath(was, "/features/", "/culture/features/")
		if got := after[uid]; got != want {
			t.Errorf("the category that was at %q is now at %q, want %q", was, got, want)
		}
	}

	// Spelled out, so that a failure names the tree rather than a uid.
	for was, want := range map[string]string{
		"/":                          "/",
		"/culture/":                  "/culture/",
		"/features/":                 "/culture/features/",
		"/features/film/":            "/culture/features/film/",
		"/features/books/":           "/culture/features/books/",
		"/features/film/reviews/":    "/culture/features/film/reviews/",
		"/features/film/interviews/": "/culture/features/film/interviews/",
	} {
		if got := after[f.cats[was].UID]; got != want {
			t.Errorf("%s moved to %q, want %q", was, got, want)
		}
	}

	// And every path still satisfies the invariant every reader assumes.
	for uid, path := range after {
		if err := domain.ValidatePath(path); err != nil {
			t.Errorf("category %s is at an invalid path after the move: %v", uid, err)
		}
	}
}

// TestRenameCategoryRewritesEveryDescendant is the other half of a move: the
// parent stays and the segment changes, and the subtree follows just the same.
func TestRenameCategoryRewritesEveryDescendant(t *testing.T) {
	f := newCatFixture(t)

	moved, err := f.db.MoveCategory(t.Context(), f.cats["/features/film/"].ID,
		domain.CategoryMove{Directory: domain.Ref("cinema")},
		f.event(f.author.ID, events.CategoryMoved))
	if err != nil {
		t.Fatalf("MoveCategory: %v", err)
	}
	if moved.Path != "/features/cinema/" {
		t.Errorf("the renamed category is at %q, want %q", moved.Path, "/features/cinema/")
	}

	after := f.paths(t)
	for was, want := range map[string]string{
		"/features/film/":            "/features/cinema/",
		"/features/film/reviews/":    "/features/cinema/reviews/",
		"/features/film/interviews/": "/features/cinema/interviews/",
		"/features/books/":           "/features/books/",
		"/features/":                 "/features/",
	} {
		if got := after[f.cats[was].UID]; got != want {
			t.Errorf("%s is now at %q, want %q", was, got, want)
		}
	}
}

// TestSubtreeRewriteSeeksOnTheSitePathIndex is the other half of
// acceptance 1: the rewrite is one statement, and it is one statement that
// seeks rather than one that reads the table.
//
// The single-statement property is a shape the code has and EXPLAIN cannot
// see; what EXPLAIN can see is whether the statement is one a site with a
// hundred thousand categories could afford. UNIQUE (site_id, path) is the
// index it seeks on, and it is the same index that makes "one root per site"
// true -- which is why there is no second index here for the prefix match.
func TestSubtreeRewriteSeeksOnTheSitePathIndex(t *testing.T) {
	f := newCatFixture(t)

	var plan []string
	err := f.db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn, `
			EXPLAIN QUERY PLAN
			UPDATE categories
			   SET path = :new_prefix || SUBSTR(path, LENGTH(:old_prefix) + 1)
			 WHERE id IN (
			       SELECT id FROM categories
			        WHERE site_id = :site_id
			          AND SUBSTR(path, 1, LENGTH(:old_prefix)) = :old_prefix
			        ORDER BY LENGTH(path) DESC)`, &sqlitex.ExecOptions{
			Named: map[string]any{
				":new_prefix": "/culture/features/",
				":old_prefix": "/features/",
				":site_id":    f.siteID,
			},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				plan = append(plan, stmt.ColumnText(3))
				return nil
			},
		})
	})
	if err != nil {
		t.Fatalf("explaining the rewrite: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN returned nothing; nothing was asserted")
	}

	var seeks bool
	for _, line := range plan {
		trimmed := strings.TrimSpace(strings.TrimLeft(line, "|-`"))
		if strings.HasPrefix(trimmed, "SEARCH categories") && strings.Contains(trimmed, "site_id=?") {
			seeks = true
		}
		// A full scan of the table is the failure this test exists to catch:
		// a move would then cost the whole site rather than the subtree.
		if trimmed == "SCAN categories" {
			t.Errorf("the subtree rewrite scans the categories table:\n%s", strings.Join(plan, "\n"))
		}
	}
	if !seeks {
		t.Errorf("the subtree rewrite does not seek on (site_id, path):\n%s", strings.Join(plan, "\n"))
	}
}

// TestMoveCategoryRefusals covers what a move may not do. The rules live in
// domain.CategoryMove.Resolve; this is that they are reached from here.
func TestMoveCategoryRefusals(t *testing.T) {
	f := newCatFixture(t)

	t.Run("a root cannot be moved", func(t *testing.T) {
		_, err := f.db.MoveCategory(t.Context(), f.root.ID,
			domain.CategoryMove{ParentPath: domain.Ref("/culture/")},
			f.event(f.author.ID, events.CategoryMoved))
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("moving a site's root = %v, want a conflict", err)
		}
	})

	t.Run("nothing moves inside itself", func(t *testing.T) {
		_, err := f.db.MoveCategory(t.Context(), f.cats["/features/"].ID,
			domain.CategoryMove{ParentPath: domain.Ref("/features/film/")},
			f.event(f.author.ID, events.CategoryMoved))
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("moving a category into its own subtree = %v, want a conflict", err)
		}
	})

	t.Run("a move onto an occupied path is a conflict", func(t *testing.T) {
		// /features/books/ renamed to "film", where /features/film/ already is.
		_, err := f.db.MoveCategory(t.Context(), f.cats["/features/books/"].ID,
			domain.CategoryMove{Directory: domain.Ref("film")},
			f.event(f.author.ID, events.CategoryMoved))
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("moving onto an existing path = %v, want a conflict", err)
		}
	})

	t.Run("a move that changes nothing is refused", func(t *testing.T) {
		_, err := f.db.MoveCategory(t.Context(), f.cats["/features/"].ID,
			domain.CategoryMove{ParentPath: domain.Ref("/")},
			f.event(f.author.ID, events.CategoryMoved))
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("moving a category to where it already is = %v, want a conflict", err)
		}
	})

	// Whatever was refused, the tree is unchanged: every refusal is inside a
	// transaction that rolled back.
	after := f.paths(t)
	for path, c := range f.cats {
		if got := after[c.UID]; got != path {
			t.Errorf("a refused move moved %q to %q", path, got)
		}
	}
}

// TestDeleteCategoryRefusesWhatDependsOnIt is why a delete is not a cascade:
// cascading would silently unfile documents, changing their URIs and stopping
// their category-scoped grants matching, and the person who deleted a section
// would find out from a reader.
func TestDeleteCategoryRefusesWhatDependsOnIt(t *testing.T) {
	f := newCatFixture(t)

	t.Run("a category with children", func(t *testing.T) {
		err := f.db.DeleteCategory(t.Context(), f.cats["/features/film/"].ID,
			f.event(f.author.ID, events.CategoryDeleted))
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("deleting a category with children = %v, want a conflict", err)
		}
	})

	t.Run("a site's root", func(t *testing.T) {
		err := f.db.DeleteCategory(t.Context(), f.root.ID,
			f.event(f.author.ID, events.CategoryDeleted))
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("deleting a site's root = %v, want a conflict", err)
		}
	})

	t.Run("a category with documents filed in it", func(t *testing.T) {
		doc, _ := f.create(t, "Filed Here")
		if _, err := f.db.SetDocumentCategories(t.Context(), doc.ID,
			[]int64{f.cats["/features/books/"].ID}, f.now,
			f.event(f.author.ID, events.DocumentFiled)); err != nil {
			t.Fatalf("SetDocumentCategories: %v", err)
		}
		err := f.db.DeleteCategory(t.Context(), f.cats["/features/books/"].ID,
			f.event(f.author.ID, events.CategoryDeleted))
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("deleting a category with documents in it = %v, want a conflict", err)
		}
	})

	t.Run("a leaf with nothing in it", func(t *testing.T) {
		if err := f.db.DeleteCategory(t.Context(), f.cats["/features/film/reviews/"].ID,
			f.event(f.author.ID, events.CategoryDeleted)); err != nil {
			t.Errorf("deleting an empty leaf: %v", err)
		}
		if _, err := f.db.CategoryByPath(t.Context(), f.siteID, "/features/film/reviews/"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the deleted category is still there: %v", err)
		}
		// The event was written before the row went, against the id it names,
		// so a deleted category still has a history.
		got, err := f.db.EventsForSubject(t.Context(), domain.SubjectCategory,
			f.cats["/features/film/reviews/"].ID, 10)
		if err != nil {
			t.Fatalf("EventsForSubject: %v", err)
		}
		var deleted bool
		for _, e := range got {
			if e.Type == events.CategoryDeleted {
				deleted = true
			}
		}
		if !deleted {
			t.Error("a deleted category has no deletion event; its history went with the row")
		}
	})
}

// TestSetDocumentCategories covers the filing itself: it replaces, the first is
// the primary, and the primary is what a document read joins in.
func TestSetDocumentCategories(t *testing.T) {
	f := newCatFixture(t)
	doc, _ := f.create(t, "A Story")

	filings, err := f.db.SetDocumentCategories(t.Context(), doc.ID,
		[]int64{f.cats["/features/film/"].ID, f.cats["/features/"].ID}, f.now,
		f.event(f.author.ID, events.DocumentFiled))
	if err != nil {
		t.Fatalf("SetDocumentCategories: %v", err)
	}
	if len(filings) != 2 {
		t.Fatalf("the document is filed in %d categories, want 2", len(filings))
	}
	primary, ok := domain.PrimaryOf(filings)
	if !ok || primary.Path != "/features/film/" {
		t.Errorf("the primary category is %q, want /features/film/", primary.Path)
	}

	// The document read joins the primary category in, which is what makes a
	// category-scoped grant resolvable and what %{categories} expands from.
	got, err := f.db.DocumentByUID(t.Context(), doc.UID)
	if err != nil {
		t.Fatalf("DocumentByUID: %v", err)
	}
	if got.CategoryPath != "/features/film/" {
		t.Errorf("the document's category path is %q, want /features/film/", got.CategoryPath)
	}
	if got.Subject().CategoryPath != "/features/film/" {
		t.Error("the document's subject carries no category path; a category-scoped grant cannot resolve against it")
	}

	// Replacing, not adding.
	filings, err = f.db.SetDocumentCategories(t.Context(), doc.ID,
		[]int64{f.cats["/culture/"].ID}, f.now, f.event(f.author.ID, events.DocumentFiled))
	if err != nil {
		t.Fatalf("SetDocumentCategories: %v", err)
	}
	if len(filings) != 1 || filings[0].Category.Path != "/culture/" || !filings[0].Primary {
		t.Errorf("after replacing, the filing is %+v; want just /culture/, primary", filings)
	}

	// Filed nowhere is a document too, and its category path is empty.
	if _, err := f.db.SetDocumentCategories(t.Context(), doc.ID, nil, f.now,
		f.event(f.author.ID, events.DocumentFiled)); err != nil {
		t.Fatalf("SetDocumentCategories(nil): %v", err)
	}
	got, err = f.db.DocumentByUID(t.Context(), doc.UID)
	if err != nil {
		t.Fatalf("DocumentByUID: %v", err)
	}
	if got.CategoryPath != "" {
		t.Errorf("a document filed nowhere reports category %q", got.CategoryPath)
	}
}

// TestCategorySubtree covers the prefix read, and the reason it is SUBSTR
// rather than LIKE: a directory may contain '%' or '_', which LIKE reads as
// wildcards.
func TestCategorySubtree(t *testing.T) {
	f := newCatFixture(t)

	got, err := f.db.CategorySubtree(t.Context(), f.siteID, "/features/")
	if err != nil {
		t.Fatalf("CategorySubtree: %v", err)
	}
	want := []string{
		"/features/", "/features/books/", "/features/film/",
		"/features/film/interviews/", "/features/film/reviews/",
	}
	if len(got) != len(want) {
		t.Fatalf("the subtree has %d categories, want %d: %v", len(got), len(want), got)
	}
	for i, c := range got {
		if c.Path != want[i] {
			t.Errorf("subtree[%d] = %q, want %q", i, c.Path, want[i])
		}
	}

	// A sibling with a shared prefix is not in the subtree, which is the
	// property the trailing slash exists for.
	f.add(t, "/", "features-and-analysis")
	got, err = f.db.CategorySubtree(t.Context(), f.siteID, "/features/")
	if err != nil {
		t.Fatalf("CategorySubtree: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("a sibling sharing a prefix joined the subtree: %v", got)
	}
}
