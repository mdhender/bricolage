// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// The category use cases (PLAN.md M7).
//
// Categories are administered through the same scoped grants documents are,
// and that is the decision worth stating. A category is not an "admin object"
// scoped by role alone (DESIGN.md 7.1): it is a place on a site, it has a
// path, and a grant with a category constraint already means something about
// it. So the privilege needed to create a category under /features is Create
// over the subject {site, /features/} -- the same question, asked of the same
// resolver, that "may this person create a story there" asks.
//
// The consequence is the one worth having: a section editor granted Create
// over /features can organise /features and cannot touch /business, with no
// second permission model to configure.

// CategoryAdmin is the privilege category administration needs.
//
// Create rather than Edit, because creating a category is creating something,
// and because the scale is ordered: whoever may create documents in a section
// may organise the section, and Edit alone -- a writer's privilege -- may not.
const CategoryAdmin = domain.Create

// CreateCategory creates a category under its parent.
//
// The privilege is resolved against the parent, because that is where the
// category is about to exist, which is the same reason CreateDocument resolves
// against a document that does not exist yet (DESIGN.md 7.2).
func (s *Service) CreateCategory(ctx context.Context, actor domain.Identity, in domain.NewCategory) (domain.Category, error) {
	if err := in.Validate(); err != nil {
		return domain.Category{}, err
	}
	parent, err := s.db.CategoryByPath(ctx, in.SiteID, in.ParentPath)
	if err != nil {
		return domain.Category{}, err
	}
	if err := s.mayAdminCategory(actor, parent); err != nil {
		return domain.Category{}, err
	}

	now := s.Now()
	uid, err := ids.New(now)
	if err != nil {
		return domain.Category{}, err
	}
	return s.db.CreateCategory(ctx, store.NewCategory{
		UID:       uid,
		ParentID:  parent.ID,
		Directory: in.Directory,
		Name:      in.Name,
		Event: domain.Event{
			Type:    events.CategoryCreated,
			ActorID: actor.User.ID,
			Payload: map[string]any{
				"uid":       uid,
				"site":      parent.SiteID,
				"parent":    parent.Path,
				"directory": in.Directory,
				"name":      in.Name,
			},
			OccurredAt: now,
		},
	})
}

// MoveCategory moves or renames a category, rewriting every descendant's path
// (PLAN.md M7 acceptance 1).
//
// Both ends are checked. A person who may organise /features and not
// /business may not move a section out of one into the other, in either
// direction: moving it out takes it away from where they may work, and moving
// it in puts documents somebody else administers under their grant. This is
// the same shape as the anti-escalation rule on grants (DESIGN.md 7.3) --
// a move is a change to two scopes, so it needs the privilege over both.
func (s *Service) MoveCategory(ctx context.Context, actor domain.Identity, uid string, move domain.CategoryMove) (domain.Category, error) {
	if move.IsEmpty() {
		return domain.Category{}, fmt.Errorf("move: nothing to change: %w", domain.ErrInvalid)
	}
	if move.Directory != nil {
		if err := domain.ValidateDirectory(*move.Directory); err != nil {
			return domain.Category{}, err
		}
	}
	c, err := s.db.CategoryByUID(ctx, uid)
	if err != nil {
		return domain.Category{}, err
	}
	if err := s.mayAdminCategory(actor, c); err != nil {
		return domain.Category{}, err
	}
	if move.ParentPath != nil {
		parent, err := s.db.CategoryByPath(ctx, c.SiteID, *move.ParentPath)
		if err != nil {
			return domain.Category{}, err
		}
		if err := s.mayAdminCategory(actor, parent); err != nil {
			return domain.Category{}, err
		}
	}

	now := s.Now()
	moved, err := s.db.MoveCategory(ctx, c.ID, move, domain.Event{
		Type:    events.CategoryMoved,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":  c.UID,
			"site": c.SiteID,
			"from": c.Path,
		},
		OccurredAt: now,
	})
	if err != nil {
		return domain.Category{}, err
	}
	return moved, nil
}

// DeleteCategory removes a category that nothing depends on.
func (s *Service) DeleteCategory(ctx context.Context, actor domain.Identity, uid string) error {
	c, err := s.db.CategoryByUID(ctx, uid)
	if err != nil {
		return err
	}
	if err := s.mayAdminCategory(actor, c); err != nil {
		return err
	}
	now := s.Now()
	return s.db.DeleteCategory(ctx, c.ID, domain.Event{
		Type:    events.CategoryDeleted,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid": c.UID, "site": c.SiteID, "path": c.Path, "name": c.Name,
		},
		OccurredAt: now,
	})
}

// Categories returns one site's categories, in path order, filtered to what
// the caller may read.
//
// Filtering here rather than in SQL is the same rule the queue follows: the
// authorization rule is authz.Resolve, a pure function over the grants the
// caller already holds, and a second implementation of it in a WHERE clause is
// a second implementation that will disagree.
func (s *Service) Categories(ctx context.Context, actor domain.Identity, siteID int64) ([]domain.Category, error) {
	all, err := s.db.ListCategories(ctx, siteID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Category, 0, len(all))
	for _, c := range all {
		if authz.Allows(actor.Grants, categorySubject(c), domain.Read) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Category reads one category.
func (s *Service) Category(ctx context.Context, actor domain.Identity, uid string) (domain.Category, error) {
	c, err := s.db.CategoryByUID(ctx, uid)
	if err != nil {
		return domain.Category{}, err
	}
	if !authz.Allows(actor.Grants, categorySubject(c), domain.Read) {
		return domain.Category{}, fmt.Errorf("category %q: %w", uid, domain.ErrNotFound)
	}
	return c, nil
}

// categorySubject renders a category as the thing a privilege is resolved
// against.
//
// It is the document subject with the document left out: the site and the
// path, which are the two dimensions a category has. A grant naming a kind, a
// workflow, a state or a document does not match it, which is right -- those
// are constraints about content and a category is not content.
func categorySubject(c domain.Category) domain.Subject {
	return domain.Subject{SiteID: c.SiteID, CategoryPath: c.Path}
}

// mayAdminCategory resolves the privilege category administration needs
// against one category.
func (s *Service) mayAdminCategory(actor domain.Identity, c domain.Category) error {
	if authz.Allows(actor.Grants, categorySubject(c), CategoryAdmin) {
		return nil
	}
	// A caller who may not even read it is told it is not there, so that the
	// API does not confirm the shape of a site to people who may not see it.
	if authz.Allows(actor.Grants, categorySubject(c), domain.Read) {
		return fmt.Errorf("category %q: %s is required: %w", c.Path, CategoryAdmin, domain.ErrForbidden)
	}
	return fmt.Errorf("category %q: %w", c.Path, domain.ErrNotFound)
}

// DocumentCategories returns where a document is filed.
func (s *Service) DocumentCategories(ctx context.Context, actor domain.Identity, uid string) (domain.Document, []domain.Filing, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return domain.Document{}, nil, err
	}
	filings, err := s.db.CategoriesForDocument(ctx, doc.ID)
	return doc, filings, err
}

// FileDocument replaces the categories a document is filed in.
//
// It needs Edit over the document and deliberately not the edit lease, which
// is the rule M5 established for assignment and due dates. The lease protects
// the working draft; document_categories rows point at the document rather
// than at a version, so there is no draft copy of them to protect, and
// requiring a checkout to refile a story would make the most ordinary bulk
// operation a newsroom performs -- moving a section -- impossible while
// anybody was writing in it.
//
// The first category given is the primary one, which is what the URI is built
// from. Filing a document somewhere is therefore a change of address, and the
// event says so.
//
// Both ends are checked, as with a move: the caller must be allowed to edit
// the document where it is now and where it is going. Otherwise refiling would
// be a way to move a document into a category whose grants the caller does not
// hold, or out of one they do.
func (s *Service) FileDocument(ctx context.Context, actor domain.Identity, uid string, paths []string) (domain.Document, []domain.Filing, error) {
	doc, err := s.mayEdit(ctx, actor, uid)
	if err != nil {
		return domain.Document{}, nil, err
	}

	categoryIDs := make([]int64, 0, len(paths))
	rendered := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			return domain.Document{}, nil, fmt.Errorf(
				"category %q is named twice: %w", path, domain.ErrInvalid)
		}
		seen[path] = true

		c, err := s.db.CategoryByPath(ctx, doc.SiteID, path)
		if err != nil {
			return domain.Document{}, nil, err
		}
		// The document is about to be there, so the privilege is resolved
		// against where it is going.
		moving := doc.Subject()
		moving.CategoryPath = c.Path
		if !authz.Allows(actor.Grants, moving, domain.Edit) {
			return domain.Document{}, nil, fmt.Errorf(
				"filing %s in %q: %s is required there: %w", uid, c.Path, domain.Edit, domain.ErrForbidden)
		}
		categoryIDs = append(categoryIDs, c.ID)
		rendered = append(rendered, c.Path)
	}

	primary := ""
	if len(rendered) > 0 {
		primary = rendered[0]
	}
	now := s.Now()
	filings, err := s.db.SetDocumentCategories(ctx, doc.ID, categoryIDs, now, domain.Event{
		Type:    events.DocumentFiled,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":          doc.UID,
			"from":         doc.CategoryPath,
			"primary":      primary,
			"categories":   rendered,
			"was_filed_in": doc.CategoryPath != "",
		},
		OccurredAt: now,
	})
	if err != nil {
		return domain.Document{}, nil, err
	}
	doc, err = s.db.DocumentByUID(ctx, uid)
	return doc, filings, err
}
