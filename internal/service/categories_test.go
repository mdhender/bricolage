// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// The service half of M7. These tests assert on emitted events as well as on
// returned values: an operation that does not write its event is not finished
// (invariant 7).

// catHarness is a service with one element type, one administrator, and the
// category tree the M7 tests share.
type catHarness struct {
	*harness
	owner domain.Identity
	cats  map[string]domain.Category
}

func newCatHarness(t *testing.T) *catHarness {
	t.Helper()
	h, _ := newDocHarness(t)
	c := &catHarness{harness: h, cats: map[string]domain.Category{}}
	c.owner = h.admin(t, "admin@example.com", "correct horse battery", domain.Publish)

	root, err := h.db.CategoryByPath(t.Context(), h.siteID, domain.RootPath)
	if err != nil {
		t.Fatalf("the seeded site has no root category: %v", err)
	}
	c.cats["/"] = root

	for _, spec := range []struct{ parent, directory string }{
		{"/", "features"},
		{"/", "business"},
		{"/features/", "film"},
		{"/features/", "books"},
		{"/features/film/", "reviews"},
	} {
		cat, err := h.CreateCategory(t.Context(), c.owner, domain.NewCategory{
			SiteID:     h.siteID,
			ParentPath: spec.parent,
			Directory:  spec.directory,
			Name:       strings.ToUpper(spec.directory[:1]) + spec.directory[1:],
		})
		if err != nil {
			t.Fatalf("CreateCategory(%s%s): %v", spec.parent, spec.directory, err)
		}
		c.cats[cat.Path] = cat
	}
	return c
}

// filed creates a document and files it in one category.
func (h *catHarness) filed(t *testing.T, title, path string) DocumentView {
	t.Helper()
	view := h.newDoc(t, h.owner, title)
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{path}); err != nil {
		t.Fatalf("FileDocument(%s, %s): %v", title, path, err)
	}
	got, err := h.Document(t.Context(), h.owner, view.Document.UID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	return got
}

// TestCreateCategoryWritesItsEvent covers the ordinary path and invariant 7.
func TestCreateCategoryWritesItsEvent(t *testing.T) {
	h := newCatHarness(t)

	c := h.cats["/features/film/"]
	if c.Path != "/features/film/" {
		t.Fatalf("the created category is at %q", c.Path)
	}
	if c.ParentID != h.cats["/features/"].ID {
		t.Errorf("the category's parent is %d, want %d", c.ParentID, h.cats["/features/"].ID)
	}

	got, err := h.db.EventsForSubject(t.Context(), domain.SubjectCategory, c.ID, 10)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(got) != 1 || got[0].Type != events.CategoryCreated {
		t.Fatalf("the category's history is %v, want one %s", got, events.CategoryCreated)
	}
	if got[0].Payload["parent"] != "/features/" {
		t.Errorf("the event payload does not name the parent: %v", got[0].Payload)
	}
}

// TestMoveCategoryWritesItsEvent is the service half of PLAN.md M7
// acceptance 1: the move rewrites the subtree, and it says so.
func TestMoveCategoryWritesItsEvent(t *testing.T) {
	h := newCatHarness(t)

	moved, err := h.MoveCategory(t.Context(), h.owner, h.cats["/features/film/"].UID,
		domain.CategoryMove{ParentPath: domain.Ref("/business/")})
	if err != nil {
		t.Fatalf("MoveCategory: %v", err)
	}
	if moved.Path != "/business/film/" {
		t.Errorf("the moved category is at %q, want /business/film/", moved.Path)
	}

	// The descendant followed.
	child, err := h.Category(t.Context(), h.owner, h.cats["/features/film/reviews/"].UID)
	if err != nil {
		t.Fatalf("Category: %v", err)
	}
	if child.Path != "/business/film/reviews/" {
		t.Errorf("the descendant is at %q, want /business/film/reviews/", child.Path)
	}

	got := h.eventsOfType(t, events.CategoryMoved)
	if len(got) != 1 {
		t.Fatalf("%d %s events, want 1", len(got), events.CategoryMoved)
	}
	if got[0].Payload["from"] != "/features/film/" {
		t.Errorf("the event does not record where it came from: %v", got[0].Payload)
	}
}

// TestFileDocumentMovesItsAddress is the point of filing: a document's URI is
// built from its primary category, so refiling is a change of address.
func TestFileDocumentMovesItsAddress(t *testing.T) {
	h := newCatHarness(t)
	view := h.newDoc(t, h.owner, "A Story")
	uid := view.Document.UID

	doc, filings, err := h.FileDocument(t.Context(), h.owner, uid,
		[]string{"/features/film/", "/features/"})
	if err != nil {
		t.Fatalf("FileDocument: %v", err)
	}
	if len(filings) != 2 {
		t.Fatalf("the document is filed in %d categories, want 2", len(filings))
	}
	if doc.CategoryPath != "/features/film/" {
		t.Errorf("the primary category is %q, want the first one given", doc.CategoryPath)
	}

	got := h.eventsOfType(t, events.DocumentFiled)
	if len(got) != 1 {
		t.Fatalf("%d %s events, want 1", len(got), events.DocumentFiled)
	}
	if got[0].Payload["primary"] != "/features/film/" {
		t.Errorf("the event does not name the primary category: %v", got[0].Payload)
	}

	t.Run("filing does not need the edit lease", func(t *testing.T) {
		// The lease protects the working draft; a filing points at the
		// document rather than at a version. Somebody else holding the
		// document must not stop a section being reorganised.
		other := h.harness.admin(t, "other@example.com", "correct horse battery", domain.Publish)
		if _, err := h.Checkout(t.Context(), other, uid); err != nil {
			t.Fatalf("Checkout: %v", err)
		}
		if _, _, err := h.FileDocument(t.Context(), h.owner, uid, []string{"/business/"}); err != nil {
			t.Errorf("filing a document somebody else has checked out: %v", err)
		}
	})

	t.Run("the same category twice is refused", func(t *testing.T) {
		_, _, err := h.FileDocument(t.Context(), h.owner, uid, []string{"/features/", "/features/"})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("naming a category twice = %v, want invalid", err)
		}
	})

	t.Run("a category that does not exist is refused", func(t *testing.T) {
		_, _, err := h.FileDocument(t.Context(), h.owner, uid, []string{"/nowhere/"})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("filing in a category that does not exist = %v, want not found", err)
		}
	})
}

// TestCategoryDeepGrantsResolveAgainstRealCategories is PLAN.md M7
// acceptance 6.
//
// The M2 tests resolved category_deep against a path typed into a table. This
// resolves it against a path the database materialised, carried by a grant the
// service wrote, loaded by the query that joins the category in, against a
// document the store read. Every one of those is a place the id and the path
// could have come apart, and the resolver is the same pure function either
// way -- which is the property the milestone is claiming.
func TestCategoryDeepGrantsResolveAgainstRealCategories(t *testing.T) {
	h := newCatHarness(t)

	inFilm := h.filed(t, "A Film Piece", "/features/film/")
	inFeatures := h.filed(t, "A Feature", "/features/")
	inBusiness := h.filed(t, "A Business Story", "/business/")
	nowhere := h.newDoc(t, h.owner, "Filed Nowhere")

	// grantee makes a user holding one category-scoped grant, written through
	// the service so that the path is resolved to a row by the same code a
	// request would use.
	grantee := func(t *testing.T, email, path string, deep bool) domain.Identity {
		t.Helper()
		role, err := h.db.CreateRole(t.Context(), email, "Role for "+email)
		if err != nil {
			t.Fatalf("CreateRole: %v", err)
		}
		if _, err := h.CreateGrant(t.Context(), h.owner, GrantRequest{
			RoleSlug:  role.Slug,
			Privilege: domain.Edit,
			Scope: domain.Scope{
				SiteID:       domain.Ref(h.siteID),
				CategoryDeep: deep,
			},
			CategoryPath: path,
		}); err != nil {
			t.Fatalf("CreateGrant(%s, deep=%t): %v", path, deep, err)
		}
		u, err := h.CreateUser(t.Context(), NewUser{
			Email: email, Name: "Test User", Password: "correct horse battery",
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := h.db.AssignRole(t.Context(), u.ID, role.ID); err != nil {
			t.Fatalf("AssignRole: %v", err)
		}
		identity, err := h.db.Identity(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("Identity: %v", err)
		}
		return identity
	}

	t.Run("the grant carries the path the database materialised", func(t *testing.T) {
		who := grantee(t, "deep@example.com", "/features/", true)
		if len(who.Grants) != 1 {
			t.Fatalf("the user holds %d grants, want 1", len(who.Grants))
		}
		scope := who.Grants[0].Scope
		if scope.CategoryID == nil || scope.CategoryPath == nil {
			t.Fatalf("the grant's scope is %+v; a category constraint needs both the id and the path", scope)
		}
		if *scope.CategoryID != h.cats["/features/"].ID {
			t.Errorf("the grant names category %d, want %d", *scope.CategoryID, h.cats["/features/"].ID)
		}
		if *scope.CategoryPath != "/features/" {
			t.Errorf("the grant carries path %q, want /features/", *scope.CategoryPath)
		}
		if !scope.CategoryDeep {
			t.Error("category_deep did not survive the round trip")
		}
	})

	t.Run("category_deep matches a descendant", func(t *testing.T) {
		who := grantee(t, "deep2@example.com", "/features/", true)
		for _, tc := range []struct {
			name string
			doc  domain.Document
			want bool
		}{
			{"the category itself", inFeatures.Document, true},
			{"a descendant", inFilm.Document, true},
			{"a sibling", inBusiness.Document, false},
			{"a document filed nowhere", nowhere.Document, false},
		} {
			got := authz.Allows(who.Grants, tc.doc.Subject(), domain.Edit)
			if got != tc.want {
				t.Errorf("%s (%q): edit = %t, want %t",
					tc.name, tc.doc.CategoryPath, got, tc.want)
			}
		}
	})

	t.Run("without category_deep a descendant does not match", func(t *testing.T) {
		who := grantee(t, "shallow@example.com", "/features/", false)
		if !authz.Allows(who.Grants, inFeatures.Document.Subject(), domain.Edit) {
			t.Error("a shallow grant does not cover its own category")
		}
		if authz.Allows(who.Grants, inFilm.Document.Subject(), domain.Edit) {
			t.Error("a shallow grant on /features/ covers /features/film/")
		}
	})

	t.Run("the refusal reaches the service, not just the resolver", func(t *testing.T) {
		who := grantee(t, "shallow2@example.com", "/features/", false)
		if _, err := h.Checkout(t.Context(), who, inFilm.Document.UID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("editing a document outside the grant = %v, want not found", err)
		}
		if _, err := h.Checkout(t.Context(), who, inFeatures.Document.UID); err != nil {
			t.Errorf("editing a document inside the grant: %v", err)
		}
	})

	t.Run("a category constraint needs a site", func(t *testing.T) {
		role, err := h.db.CreateRole(t.Context(), "siteless", "Siteless")
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.CreateGrant(t.Context(), h.owner, GrantRequest{
			RoleSlug:     role.Slug,
			Privilege:    domain.Edit,
			CategoryPath: "/features/",
		})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("a category grant with no site = %v, want invalid", err)
		}
	})
}

// TestCategoryAdministrationIsScoped is the decision this milestone makes about
// who may organise a site: the same scoped grants that decide who may edit its
// documents.
func TestCategoryAdministrationIsScoped(t *testing.T) {
	h := newCatHarness(t)

	role, err := h.db.CreateRole(t.Context(), "features-editor", "Features editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateGrant(t.Context(), h.owner, GrantRequest{
		RoleSlug:     role.Slug,
		Privilege:    CategoryAdmin,
		Scope:        domain.Scope{SiteID: domain.Ref(h.siteID), CategoryDeep: true},
		CategoryPath: "/features/",
	}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	u, err := h.CreateUser(t.Context(), NewUser{
		Email: "features@example.com", Name: "Features", Password: "correct horse battery",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.AssignRole(t.Context(), u.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	editor, err := h.db.Identity(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("inside the grant", func(t *testing.T) {
		if _, err := h.CreateCategory(t.Context(), editor, domain.NewCategory{
			SiteID: h.siteID, ParentPath: "/features/", Directory: "tv", Name: "TV",
		}); err != nil {
			t.Errorf("creating a category inside the grant: %v", err)
		}
	})

	t.Run("outside the grant", func(t *testing.T) {
		_, err := h.CreateCategory(t.Context(), editor, domain.NewCategory{
			SiteID: h.siteID, ParentPath: "/business/", Directory: "markets", Name: "Markets",
		})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("creating a category outside the grant = %v, want not found", err)
		}
	})

	t.Run("a move needs the privilege at both ends", func(t *testing.T) {
		// Moving /features/film/ out to /business/ is refused, because the
		// destination is outside the grant even though the source is not.
		_, err := h.MoveCategory(t.Context(), editor, h.cats["/features/film/"].UID,
			domain.CategoryMove{ParentPath: domain.Ref("/business/")})
		if err == nil {
			t.Error("a move into a category the actor may not administer was accepted")
		}
		// And moving it within the grant is fine.
		if _, err := h.MoveCategory(t.Context(), editor, h.cats["/features/film/"].UID,
			domain.CategoryMove{ParentPath: domain.Ref("/features/books/")}); err != nil {
			t.Errorf("a move inside the grant: %v", err)
		}
	})
}

// TestCheckinValidatesContent is PLAN.md M7 acceptance 5: a check-in of
// content that violates the element type's schema is refused and names the
// offending fields, and a working draft may be invalid.
func TestCheckinValidatesContent(t *testing.T) {
	h, editor := newDocHarness(t)

	view, err := h.CreateDocument(t.Context(), editor, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          "A Story",
		Content:        `{"boyd":"typo","deck":42}`,
	})
	if err != nil {
		t.Fatalf("a working draft may be invalid, so creating one must succeed: %v", err)
	}
	uid := view.Document.UID

	t.Run("a working draft may be invalid", func(t *testing.T) {
		// Created, and editable, with content that will not check in. This is
		// the half of the rule that costs somebody a sentence if it is wrong.
		if _, err := h.Checkout(t.Context(), editor, uid); err != nil {
			t.Fatalf("Checkout: %v", err)
		}
		if _, err := h.UpdateDraft(t.Context(), editor, uid, domain.DraftUpdate{
			Content: domain.Ref(`{"still":"wrong"}`),
		}); err != nil {
			t.Errorf("editing a draft into an invalid state: %v", err)
		}
	})

	t.Run("check-in names the offending fields", func(t *testing.T) {
		_, err := h.Checkin(t.Context(), editor, uid, "")
		if err == nil {
			t.Fatal("a check-in of content that violates the schema was accepted")
		}
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("the refusal is %v, want it to wrap ErrInvalid (a 422 at the edge)", err)
		}
		fields, ok := domain.FieldErrorsOf(err)
		if !ok {
			t.Fatalf("the refusal is %v, want a *ContentError carrying the offending fields", err)
		}
		if len(fields) != 1 || fields[0].Field != "still" {
			t.Errorf("the refusal names %v, want just \"still\"", fields)
		}
	})

	t.Run("nothing was written", func(t *testing.T) {
		// The refusal happens before the store is touched, so the draft is
		// still open and the lease is still held.
		draft, err := h.db.DraftForDocument(t.Context(), view.Document.ID)
		if err != nil {
			t.Fatalf("the working draft is gone after a refused check-in: %v", err)
		}
		if !draft.IsDraft() {
			t.Error("the draft was checked in by a refused check-in")
		}
		if got := h.eventsOfType(t, events.DocumentCheckedIn); len(got) != 0 {
			t.Errorf("a refused check-in wrote %d %s events", len(got), events.DocumentCheckedIn)
		}
	})

	t.Run("fixing the content lets it in", func(t *testing.T) {
		if _, err := h.UpdateDraft(t.Context(), editor, uid, domain.DraftUpdate{
			Content: domain.Ref(`{"body":"Words.","deck":"A deck."}`),
		}); err != nil {
			t.Fatalf("UpdateDraft: %v", err)
		}
		if _, err := h.Checkin(t.Context(), editor, uid, "fixed"); err != nil {
			t.Errorf("Checkin: %v", err)
		}
	})
}

// TestOutputChannelAdministration covers the privilege and the defaults.
func TestOutputChannelAdministration(t *testing.T) {
	h := newCatHarness(t)

	oc, err := h.CreateOutputChannel(t.Context(), h.owner, domain.OutputChannel{
		SiteID: h.siteID, Name: "Web", UseSlug: true,
	})
	if err != nil {
		t.Fatalf("CreateOutputChannel: %v", err)
	}
	if oc.URIFormat != domain.DefaultURIFormat {
		t.Errorf("uri_format = %q, want the default %q", oc.URIFormat, domain.DefaultURIFormat)
	}
	if oc.URICase != domain.URICaseMixed || oc.Filename != domain.DefaultFilename {
		t.Errorf("the defaults were not filled in: %+v", oc)
	}
	if got := h.eventsOfType(t, events.OutputChannelCreated); len(got) != 1 {
		t.Errorf("%d %s events, want 1", len(got), events.OutputChannelCreated)
	}

	t.Run("a format naming a conversion nobody implements is refused", func(t *testing.T) {
		_, err := h.CreateOutputChannel(t.Context(), h.owner, domain.OutputChannel{
			SiteID: h.siteID, Name: "Broken", URIFormat: domain.TokenCategories + "/%Q",
		})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("a bad format = %v, want invalid at configuration time", err)
		}
	})

	t.Run("writing one needs publish over the site", func(t *testing.T) {
		editor := h.harness.admin(t, "just-an-editor@example.com", "correct horse battery", domain.Create)
		_, err := h.CreateOutputChannel(t.Context(), editor, domain.OutputChannel{
			SiteID: h.siteID, Name: "Editor's",
		})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("an editor writing an output channel = %v, want forbidden", err)
		}
	})

	t.Run("an update replaces the configuration", func(t *testing.T) {
		updated, err := h.UpdateOutputChannel(t.Context(), h.owner, oc.UID, domain.OutputChannel{
			Name: "Web", URICase: domain.URICaseLower, UseSlug: false,
		})
		if err != nil {
			t.Fatalf("UpdateOutputChannel: %v", err)
		}
		if updated.URICase != domain.URICaseLower || updated.UseSlug {
			t.Errorf("the update did not take: %+v", updated)
		}
		if updated.SiteID != oc.SiteID || updated.UID != oc.UID {
			t.Error("the update changed the site or the uid; neither is changeable")
		}
	})
}

// TestDocumentURIs covers the visible face of domain.BuildURI: the four things
// it takes are fetched here, and a channel that cannot build one reports its
// reason rather than failing the request.
func TestDocumentURIs(t *testing.T) {
	h := newCatHarness(t)

	if _, err := h.CreateOutputChannel(t.Context(), h.owner, domain.OutputChannel{
		SiteID: h.siteID, Name: "Web", UseSlug: true,
	}); err != nil {
		t.Fatalf("CreateOutputChannel: %v", err)
	}

	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          "A Film Piece",
		Slug:           "a-film-piece",
		CoverDate:      "2026-03-01",
		Content:        `{"body":"Words."}`,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	uid := view.Document.UID

	t.Run("a document filed nowhere has no address", func(t *testing.T) {
		_, _, err := h.DocumentURIs(t.Context(), h.owner, uid)
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("the URIs of an unfiled document = %v, want a conflict saying so", err)
		}
	})

	if _, _, err := h.FileDocument(t.Context(), h.owner, uid, []string{"/features/film/"}); err != nil {
		t.Fatalf("FileDocument: %v", err)
	}

	t.Run("filed, it has one", func(t *testing.T) {
		_, uris, err := h.DocumentURIs(t.Context(), h.owner, uid)
		if err != nil {
			t.Fatalf("DocumentURIs: %v", err)
		}
		if len(uris) != 1 {
			t.Fatalf("%d addresses, want 1", len(uris))
		}
		if uris[0].Err != nil {
			t.Fatalf("the address could not be built: %v", uris[0].Err)
		}
		if want := "/features/film/2026/03/01/a-film-piece"; uris[0].URI != want {
			t.Errorf("uri = %q, want %q", uris[0].URI, want)
		}
		if want := "/features/film/2026/03/01/a-film-piece/index.html"; uris[0].File != want {
			t.Errorf("file = %q, want %q", uris[0].File, want)
		}
	})

	t.Run("moving the category moves the address", func(t *testing.T) {
		if _, err := h.MoveCategory(t.Context(), h.owner, h.cats["/features/film/"].UID,
			domain.CategoryMove{ParentPath: domain.Ref("/business/")}); err != nil {
			t.Fatalf("MoveCategory: %v", err)
		}
		_, uris, err := h.DocumentURIs(t.Context(), h.owner, uid)
		if err != nil {
			t.Fatalf("DocumentURIs: %v", err)
		}
		if want := "/business/film/2026/03/01/a-film-piece"; uris[0].URI != want {
			t.Errorf("uri = %q, want %q; the subtree rewrite did not reach the URI", uris[0].URI, want)
		}
	})

	t.Run("a channel that cannot build one says why", func(t *testing.T) {
		// A second document with no cover date, against a channel whose format
		// carries one. The refusal is per channel and not per request.
		noDate, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
			SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "story",
			Title: "Undated", Slug: "undated",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.FileDocument(t.Context(), h.owner, noDate.Document.UID, []string{"/business/"}); err != nil {
			t.Fatal(err)
		}
		_, uris, err := h.DocumentURIs(t.Context(), h.owner, noDate.Document.UID)
		if err != nil {
			t.Fatalf("DocumentURIs: %v", err)
		}
		if len(uris) != 1 || uris[0].Err == nil {
			t.Fatalf("a dated format against an undated version produced %+v", uris)
		}
		if uris[0].URI != "" {
			t.Errorf("a channel that refused still reported the URI %q", uris[0].URI)
		}
	})
}

// TestElementTypeAdministration covers the privilege, the schema check, and
// what may not change.
func TestElementTypeAdministration(t *testing.T) {
	h := newCatHarness(t)

	et, err := h.CreateElementType(t.Context(), h.owner, NewElementTypeRequest{
		KeyName: "page", Kind: domain.KindStory, TopLevel: true, FixedURI: true,
		Schema: `{"fields":[{"name":"body","type":"block","required":true}]}`,
	})
	if err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	if !et.FixedURI || et.Name != "page" {
		t.Errorf("the element type is %+v; the name defaults to the key name", et)
	}
	if got := h.eventsOfType(t, events.ElementTypeCreated); len(got) != 1 {
		t.Errorf("%d %s events, want 1", len(got), events.ElementTypeCreated)
	}

	t.Run("a schema declaring a type nobody implements is refused", func(t *testing.T) {
		_, err := h.CreateElementType(t.Context(), h.owner, NewElementTypeRequest{
			KeyName: "broken", Kind: domain.KindStory,
			Schema: `{"fields":[{"name":"body","type":"markdown"}]}`,
		})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("a bad schema = %v, want invalid when it is written", err)
		}
	})

	t.Run("writing one needs the system subject", func(t *testing.T) {
		// A grant scoped to a site says what its holder may do to that site's
		// documents and says nothing about a definition every site shares.
		scoped := h.userWithGrant(t, "site-admin@example.com", "correct horse battery",
			domain.Grant{Privilege: domain.Publish, Scope: domain.Scope{SiteID: domain.Ref(h.siteID)}})
		_, err := h.CreateElementType(t.Context(), scoped, NewElementTypeRequest{
			KeyName: "scoped", Kind: domain.KindStory,
		})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("a site-scoped grant wrote an element type = %v, want forbidden", err)
		}
	})

	t.Run("an update changes the schema and writes its event", func(t *testing.T) {
		updated, err := h.UpdateElementType(t.Context(), h.owner, "page", ElementTypeUpdate{
			Schema: domain.Ref(`{"fields":[{"name":"body","type":"block"},{"name":"caption","type":"text"}]}`),
		})
		if err != nil {
			t.Fatalf("UpdateElementType: %v", err)
		}
		if !strings.Contains(updated.Schema, "caption") {
			t.Errorf("the schema did not change: %s", updated.Schema)
		}
		if !updated.FixedURI {
			t.Error("an update that did not mention fixed_uri turned it off")
		}
		if got := h.eventsOfType(t, events.ElementTypeUpdated); len(got) != 1 {
			t.Errorf("%d %s events, want 1", len(got), events.ElementTypeUpdated)
		}
	})

	t.Run("an update that asks for nothing is refused", func(t *testing.T) {
		if _, err := h.UpdateElementType(t.Context(), h.owner, "page", ElementTypeUpdate{}); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("an empty update = %v, want invalid", err)
		}
	})
}

// TestCreateSiteCreatesItsRoot is the store rule seen from here: nothing else
// creates a root, so every site created after M7 has exactly one.
func TestCreateSiteCreatesItsRoot(t *testing.T) {
	h := newHarness(t)
	id, err := h.db.CreateSite(t.Context(), store.NewSite{
		UID: ids.MustNew(start), Name: "Second", Domain: "second.example.com",
	})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	root, err := h.db.CategoryByPath(t.Context(), id, domain.RootPath)
	if err != nil {
		t.Fatalf("a site created without a root category: %v", err)
	}
	if root.SiteID != id || !root.IsRoot() {
		t.Errorf("the root is %+v, want site %d and no parent", root, id)
	}
}

// TestSiteAdministration is issue #3: the site row is a development hostname
// and something has to be able to change it.
//
// The privilege, the fold, the refusal of a name that could not be a host, and
// the event that says what the domain used to be -- which is the only record
// of it, because the row shows only what it says today.
func TestSiteAdministration(t *testing.T) {
	h := newCatHarness(t)

	site, err := h.db.SiteByID(t.Context(), h.siteID)
	if err != nil {
		t.Fatalf("SiteByID: %v", err)
	}

	t.Run("the domain is folded and the event carries the old one", func(t *testing.T) {
		updated, err := h.UpdateSite(t.Context(), h.owner, site.UID, SiteUpdate{
			Domain: domain.Ref("  WWW.Example.COM  "),
			Name:   domain.Ref("The Daily Paper"),
		})
		if err != nil {
			t.Fatalf("UpdateSite: %v", err)
		}
		if updated.Domain != "www.example.com" {
			t.Errorf("the domain is %q, want it trimmed and folded", updated.Domain)
		}
		if updated.Name != "The Daily Paper" {
			t.Errorf("the name is %q", updated.Name)
		}
		if updated.UID != site.UID || updated.ID != site.ID {
			t.Error("a rename minted a new site rather than renaming the one that was there")
		}

		got := h.eventsOfType(t, events.SiteUpdated)
		if len(got) != 1 {
			t.Fatalf("%d %s events, want 1", len(got), events.SiteUpdated)
		}
		if was, _ := got[0].Payload["was_domain"].(string); was != site.Domain {
			t.Errorf("the event says the domain was %q, want %q; without it nothing records what every URL used to be",
				was, site.Domain)
		}
	})

	t.Run("the root category keeps its own name", func(t *testing.T) {
		// It is a copy made when the site was created, and a category with a
		// life of its own since. Writing it from here would undo a rename
		// somebody made deliberately.
		root, err := h.db.CategoryByPath(t.Context(), h.siteID, domain.RootPath)
		if err != nil {
			t.Fatalf("CategoryByPath: %v", err)
		}
		if root.Name != "Default" {
			t.Errorf("the root category is now named %q; renaming the site renamed it too", root.Name)
		}
	})

	t.Run("a name that could not be a host is refused", func(t *testing.T) {
		for _, bad := range []string{"", "  ", "www example com", "../etc", "a/b", "www..example.com",
			"-example.com", "example.com:", "example.com:0", "example.com:99999", "exámple.com"} {
			if _, err := h.UpdateSite(t.Context(), h.owner, site.UID, SiteUpdate{
				Domain: domain.Ref(bad),
			}); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("UpdateSite(%q) = %v, want invalid", bad, err)
			}
		}
	})

	t.Run("a port is allowed, because development serves on one", func(t *testing.T) {
		updated, err := h.UpdateSite(t.Context(), h.owner, site.UID, SiteUpdate{
			Domain: domain.Ref("assemblage.localhost:8443"),
		})
		if err != nil {
			t.Fatalf("UpdateSite: %v", err)
		}
		if updated.Domain != "assemblage.localhost:8443" {
			t.Errorf("the domain is %q", updated.Domain)
		}
	})

	t.Run("a domain another site holds is a conflict", func(t *testing.T) {
		if _, err := h.db.CreateSite(t.Context(), store.NewSite{
			UID: ids.MustNew(start), Name: "Second", Domain: "second.example.com",
		}); err != nil {
			t.Fatalf("CreateSite: %v", err)
		}
		_, err := h.UpdateSite(t.Context(), h.owner, site.UID, SiteUpdate{
			Domain: domain.Ref("second.example.com"),
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("two sites on one domain = %v, want conflict; the template tree would have two answers", err)
		}
	})

	t.Run("a site-scoped grant is not enough", func(t *testing.T) {
		// Renaming a site moves the first path segment of the template tree,
		// which is a decision about the shape of the installation rather than
		// about this site's content.
		scoped := h.userWithGrant(t, "site-publisher@example.com", "correct horse battery",
			domain.Grant{Privilege: domain.Publish, Scope: domain.Scope{SiteID: domain.Ref(h.siteID)}})
		if _, err := h.UpdateSite(t.Context(), scoped, site.UID, SiteUpdate{
			Domain: domain.Ref("hijacked.example.com"),
		}); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("a site-scoped grant renamed the site = %v, want forbidden", err)
		}
	})

	t.Run("an update that asks for nothing is refused", func(t *testing.T) {
		if _, err := h.UpdateSite(t.Context(), h.owner, site.UID, SiteUpdate{}); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("an empty update = %v, want invalid", err)
		}
	})

	t.Run("a site that is not there is not found", func(t *testing.T) {
		if _, err := h.UpdateSite(t.Context(), h.owner, "00000000000000000000000000", SiteUpdate{
			Domain: domain.Ref("nowhere.example.com"),
		}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("UpdateSite on a missing site = %v, want not found", err)
		}
	})
}
