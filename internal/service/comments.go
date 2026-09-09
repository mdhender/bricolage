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

// The comment use cases (PLAN.md M11): open a thread, reply to one, and decide
// that one is settled.
//
// Two privileges, and the split between them is the interesting decision.
// Writing a comment needs Read: raising a concern is what a fact-checker, a
// picture editor, or a lawyer does about a story they may see and may not
// touch, and a system that only lets the people who can edit a document say
// something about it has no copy desk. Resolving one needs Edit, because
// closing a thread changes what the process will allow -- GuardCommentsResolved
// refuses while any is open -- and that is an act on the document rather than
// a remark about it.
//
// The one exception is the author of a thread, who may always resolve their
// own: they raised the question, and telling somebody they may ask but not
// withdraw is a rule with no purpose behind it.

// Comments returns every thread on a document, oldest first.
//
// Read is what it needs. A discussion is part of what a document is, and
// hiding it from people who can see the document would leave them reading a
// story whose open questions are invisible.
func (s *Service) Comments(ctx context.Context, actor domain.Identity, uid string) (domain.Document, []domain.Thread, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return domain.Document{}, nil, err
	}
	comments, err := s.db.CommentsForDocument(ctx, doc.ID)
	if err != nil {
		return domain.Document{}, nil, err
	}
	return doc, domain.Threads(comments), nil
}

// Comment opens a thread on a document, or replies to one.
//
// replyTo is a comment uid, or empty to open a thread. Replying to a reply
// joins that reply's thread rather than starting a branch: threads are one
// level deep, so that "is this settled" has one answer per discussion
// (DESIGN.md 5.5).
//
// The comment records the version being looked at, which is the document's
// current version, and it is a note about what was on screen rather than a
// scope: a comment made against version 3 stays visible when version 4 exists,
// because the discussion did not stop being about the document. There is
// deliberately no way for a client to choose the version -- a comment about a
// version nobody was reading is a comment about nothing.
//
// It needs no checkout. A comment points at the document rather than at the
// working draft, and requiring the edit lease to say something about a story
// would mean the only person who could comment on it is the one person who
// cannot be reviewing it.
func (s *Service) Comment(ctx context.Context, actor domain.Identity, uid, body, replyTo string) (domain.Document, domain.Comment, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return domain.Document{}, domain.Comment{}, err
	}
	clean, err := domain.ValidateCommentBody(body)
	if err != nil {
		return domain.Document{}, domain.Comment{}, err
	}

	var root int64
	if replyTo != "" {
		parent, err := s.db.CommentByUID(ctx, replyTo)
		if err != nil {
			return domain.Document{}, domain.Comment{}, err
		}
		if parent.DocumentID != doc.ID {
			// Naming a comment on another document is a mistake rather than a
			// permission question, and answering it with that comment's
			// document would file the reply where nobody asked for it.
			return domain.Document{}, domain.Comment{}, fmt.Errorf(
				"comment %s is not on document %s: %w", replyTo, uid, domain.ErrInvalid)
		}
		root = parent.ID
		if parent.IsReply() {
			root = parent.InReplyTo
		}
	}

	now := s.Now()
	commentUID, err := ids.New(now)
	if err != nil {
		return domain.Document{}, domain.Comment{}, err
	}
	payload := map[string]any{
		"uid":     doc.UID,
		"comment": commentUID,
		"body":    clean,
	}
	if root != 0 {
		payload["reply"] = true
	}
	if doc.CurrentVersionID != 0 {
		payload["version_id"] = doc.CurrentVersionID
	}

	c, err := s.db.CreateComment(ctx, store.NewComment{
		UID:        commentUID,
		DocumentID: doc.ID,
		VersionID:  doc.CurrentVersionID,
		InReplyTo:  root,
		AuthorID:   actor.User.ID,
		Body:       clean,
		CreatedAt:  now,
	}, domain.Event{
		Type:       events.DocumentCommented,
		ActorID:    actor.User.ID,
		Payload:    payload,
		OccurredAt: now,
	})
	if err != nil {
		return domain.Document{}, domain.Comment{}, err
	}
	return doc, c, nil
}

// ResolveComment closes a thread.
//
// A reply is refused rather than quietly resolving the thread it belongs to.
// Resolution is a property of the discussion, and a client that resolved a
// reply believing it had closed a thread would be right by accident; the
// refusal names the thread to resolve instead.
//
// Resolving a thread that is already resolved is not an error. Two editors
// reading the same page and both deciding the question is settled have agreed
// rather than collided, and the answer to the second is the thread as it
// stands -- with the first editor's name on it, and no second event, because
// nothing changed.
func (s *Service) ResolveComment(ctx context.Context, actor domain.Identity, commentUID string) (domain.Document, domain.Comment, error) {
	c, err := s.db.CommentByUID(ctx, commentUID)
	if err != nil {
		return domain.Document{}, domain.Comment{}, err
	}
	doc, err := s.db.DocumentByID(ctx, c.DocumentID)
	if err != nil {
		return domain.Document{}, domain.Comment{}, err
	}
	if !authz.Allows(actor.Grants, doc.Subject(), domain.Read) {
		return domain.Document{}, domain.Comment{}, fmt.Errorf("comment %q: %w", commentUID, domain.ErrNotFound)
	}
	if c.AuthorID != actor.User.ID && !authz.Allows(actor.Grants, doc.Subject(), domain.Edit) {
		return domain.Document{}, domain.Comment{}, fmt.Errorf(
			"comment %s: %s over document %s is required to resolve somebody else's thread: %w",
			commentUID, domain.Edit, doc.UID, domain.ErrForbidden)
	}
	if c.IsReply() {
		root, err := s.db.CommentByID(ctx, c.InReplyTo)
		if err != nil {
			return domain.Document{}, domain.Comment{}, err
		}
		return domain.Document{}, domain.Comment{}, fmt.Errorf(
			"comment %s is a reply; resolve the thread it belongs to, %s: %w",
			commentUID, root.UID, domain.ErrConflict)
	}

	now := s.Now()
	resolved, changed, err := s.db.ResolveComment(ctx, c.ID, actor.User.ID, now, domain.Event{
		Type:    events.DocumentCommentResolved,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":     doc.UID,
			"comment": commentUID,
		},
		OccurredAt: now,
	})
	if err != nil {
		return domain.Document{}, domain.Comment{}, err
	}
	if !changed {
		s.log.Debug("comment already resolved",
			"comment", commentUID, "document", doc.UID, "actor", actor.User.UID)
	}
	return doc, resolved, nil
}
