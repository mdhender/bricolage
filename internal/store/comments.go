// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// All the SQL for comments (invariant 2).
//
// Comments are a thread and resolution is a property of the thread, which is
// the one thing in this file worth reading twice. GuardCommentsResolved asks
// how many discussions about this document are still open, so the count is of
// unresolved *roots*: a reply to a settled thread is not a second open
// question, and a thread with four replies is not four of them.
//
// The table landed in 0005 with the guard that reads it, one milestone before
// this API, for the reason approvals.go gives.

// commentSelect is the projection every comment read shares.
//
// The join is what keeps invariant 10 reachable from the transport: a comment
// records the version it was written against by primary key, and an internal
// integer never appears in a response, so the number a person reads has to
// come out of the same query. It is a LEFT JOIN because version_id is
// nullable -- a comment about the document as a whole is about no version --
// and a comment nobody could read because its version column was NULL would
// be a discussion lost to a join.
const commentSelect = `
	SELECT c.id, c.uid, c.document_id, c.version_id, c.in_reply_to, c.author_id, c.body,
	       c.resolved_at, c.resolved_by, c.created_at, v.version AS version_number
	  FROM comments c
	  LEFT JOIN document_versions v ON v.id = c.version_id`

// NewComment is what CreateComment is given.
type NewComment struct {
	UID        string
	DocumentID int64

	// VersionID is the version the comment is about, or 0 for a comment about
	// the document as a whole.
	VersionID int64

	// InReplyTo is the thread root this reply joins, or 0 to open a thread.
	// The caller resolves a reply-to-a-reply into its root before arriving
	// here: threads are one level deep, and which level a comment lands on is
	// a decision rather than a lookup (DESIGN.md 5.5).
	InReplyTo int64

	AuthorID  int64
	Body      string
	CreatedAt time.Time
}

// CreateComment opens a comment thread on a document, or adds a reply to one.
//
// The event is written in the same transaction (invariant 7). It carries the
// comment's uid in its payload and the document as its subject, because "what
// happened to this story" is the question a person asks of a history and a
// thread that only appeared under its own subject would be a thread nobody
// finds.
func (db *DB) CreateComment(ctx context.Context, c NewComment, event domain.Event) (domain.Comment, error) {
	var out domain.Comment
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("commenting on document %d", c.DocumentID), `
			INSERT INTO comments (uid, document_id, version_id, in_reply_to, author_id, body, created_at)
			VALUES (:uid, :document_id, :version_id, :in_reply_to, :author_id, :body, :created_at)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", c.UID)
				stmt.SetInt64(":document_id", c.DocumentID)
				if c.VersionID == 0 {
					stmt.SetNull(":version_id")
				} else {
					stmt.SetInt64(":version_id", c.VersionID)
				}
				if c.InReplyTo == 0 {
					stmt.SetNull(":in_reply_to")
				} else {
					stmt.SetInt64(":in_reply_to", c.InReplyTo)
				}
				stmt.SetInt64(":author_id", c.AuthorID)
				stmt.SetText(":body", c.Body)
				stmt.SetText(":created_at", formatTime(c.CreatedAt))
			}, nil)
		if err != nil {
			return err
		}
		id := conn.LastInsertRowID()

		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = c.DocumentID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return commentByID(conn, id, &out)
	})
	return out, err
}

// ResolveComment closes a comment thread and reports whether this call is what
// closed it.
//
// Resolving a thread that is already resolved changes nothing and writes
// nothing, which is the same answer CreateApproval gives to a second approval:
// two editors reading the same page and both deciding the question is settled
// have agreed, not collided. The row is still returned, so the caller can say
// who closed it and when.
//
// The UPDATE names resolved_at IS NULL, so the decision and the write are one
// statement: a read-then-write would let two callers both see it open and the
// second overwrite the first's name.
func (db *DB) ResolveComment(ctx context.Context, id, userID int64, now time.Time, event domain.Event) (domain.Comment, bool, error) {
	var (
		out      domain.Comment
		resolved bool
	)
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		// The comment is read first for its document, which is the event's
		// subject, and to fail with "no such comment" rather than with a
		// silent no-op when the id names nothing.
		if err := commentByID(conn, id, &out); err != nil {
			return err
		}
		err := run(conn, fmt.Sprintf("resolving comment %d", id), `
			UPDATE comments SET resolved_at = :now, resolved_by = :user_id
			 WHERE id = :id AND resolved_at IS NULL`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":now", formatTime(now))
				stmt.SetInt64(":user_id", userID)
				stmt.SetInt64(":id", id)
			}, nil)
		if err != nil {
			return err
		}
		resolved = conn.Changes() == 1
		if !resolved {
			return nil
		}

		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = out.DocumentID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return commentByID(conn, id, &out)
	})
	return out, resolved, err
}

// CommentByUID reads one comment by the identifier the API speaks
// (invariant 10).
func (db *DB) CommentByUID(ctx context.Context, uid string) (domain.Comment, error) {
	var out domain.Comment
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("comment %q", uid),
			commentSelect+` WHERE c.uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanComment(stmt)
				return err
			})
	})
	return out, err
}

// CommentByID reads one comment by its primary key. The id always comes from
// a row this process already read (invariant 10); it is exported so that a
// refusal about a reply can name the thread the reply belongs to.
func (db *DB) CommentByID(ctx context.Context, id int64) (domain.Comment, error) {
	var out domain.Comment
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return commentByID(conn, id, &out)
	})
	return out, err
}

// CommentsForDocument returns every comment on a document, oldest first.
//
// One query for the whole discussion, grouped into threads by
// domain.Threads: a document with forty comments is one read rather than
// forty-one. comments_document, from migration 0011, is the index it seeks on.
func (db *DB) CommentsForDocument(ctx context.Context, documentID int64) ([]domain.Comment, error) {
	var out []domain.Comment
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("comments on document %d", documentID),
			commentSelect+` WHERE c.document_id = :document_id ORDER BY c.id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
			func(stmt *sqlite.Stmt) error {
				c, err := scanComment(stmt)
				if err != nil {
					return err
				}
				out = append(out, c)
				return nil
			})
	})
	return out, err
}

// CountUnresolvedComments returns how many threads a document still has open.
func (db *DB) CountUnresolvedComments(ctx context.Context, documentID int64) (int, error) {
	var n int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		n, err = unresolvedComments(conn, documentID)
		return err
	})
	return n, err
}

// unresolvedComments counts the document's open threads, which is what
// GuardCommentsResolved refuses on.
//
// It counts roots. A reply is part of the discussion its root opened and is
// resolved by resolving that thread, so counting every unresolved row would
// report a thread with three replies as four open questions and would leave
// the guard permanently refusing on rows nothing can close.
//
// comments_open, from 0005, is partial on "resolved_at IS NULL", and the WHERE
// here implies it, so SQLite may still use it.
func unresolvedComments(conn *sqlite.Conn, documentID int64) (int, error) {
	var n int
	err := one(conn, fmt.Sprintf("open comment threads on document %d", documentID), `
		SELECT COUNT(*) AS n
		  FROM comments
		 WHERE document_id = :document_id AND resolved_at IS NULL AND in_reply_to IS NULL`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
		func(stmt *sqlite.Stmt) error {
			n = int(stmt.GetInt64("n"))
			return nil
		})
	return n, err
}

// commentByID reads one comment on a connection the caller holds. The id
// always comes from a row this process already read (invariant 10).
func commentByID(conn *sqlite.Conn, id int64, c *domain.Comment) error {
	return one(conn, fmt.Sprintf("comment %d", id),
		commentSelect+` WHERE c.id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*c, err = scanComment(stmt)
			return err
		})
}

func scanComment(stmt *sqlite.Stmt) (domain.Comment, error) {
	id := stmt.GetInt64("id")
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Comment{}, fmt.Errorf("comment %d: created_at: %w", id, err)
	}
	c := domain.Comment{
		ID:         id,
		UID:        stmt.GetText("uid"),
		DocumentID: stmt.GetInt64("document_id"),
		AuthorID:   stmt.GetInt64("author_id"),
		Body:       stmt.GetText("body"),
		CreatedAt:  created,
	}
	if v := nullInt64(stmt, "version_id"); v != nil {
		c.VersionID = *v
	}
	if v := nullInt64(stmt, "in_reply_to"); v != nil {
		c.InReplyTo = *v
	}
	if v := nullInt64(stmt, "version_number"); v != nil {
		c.VersionNumber = int(*v)
	}
	if v := nullText(stmt, "resolved_at"); v != nil {
		at, err := parseTime(*v)
		if err != nil {
			return domain.Comment{}, fmt.Errorf("comment %d: resolved_at: %w", id, err)
		}
		c.ResolvedAt = at
	}
	if v := nullInt64(stmt, "resolved_by"); v != nil {
		c.ResolvedBy = *v
	}
	return c, nil
}
