// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Comments are a thread (DESIGN.md 5.5, PLAN.md M11).
//
// The system we learned from had one overwritten note per version for its
// entire collaboration story, so "what did the copy desk say about this last
// week" was a question with no answer. A thread is a root comment and the
// replies hanging off it, and resolution is a property of the thread rather
// than of each message in it: GuardCommentsResolved asks how many discussions
// about this document are still open, and a reply to a settled thread is not a
// second open question.
//
// document_versions.note stays as well. A check-in message says what changed;
// a comment says what somebody thinks about it, and collapsing the two loses
// both.

// MaxCommentBody bounds one comment, in runes.
//
// It is a limit rather than an invitation: a comment is a remark on a
// document, and something longer than this is a document of its own. The
// transport bounds the whole body too (api.maxBodyBytes); this bounds the one
// field, so the refusal names it.
const MaxCommentBody = 4096

// Comment is one message in a discussion about a document.
type Comment struct {
	ID  int64
	UID string

	DocumentID int64

	// VersionID is the version the comment is about, or 0 for a comment about
	// the document as a whole. It is a note about what was being read rather
	// than a scope: a comment on version 3 stays visible when version 4
	// exists, because the discussion did not stop being about the document.
	VersionID int64

	// VersionNumber is that version's 1-based number, and 0 when there is no
	// version. It travels with the row because the API speaks numbers and
	// never internal keys (invariant 10), and deriving it at the transport
	// edge would be a second query per comment.
	VersionNumber int

	// InReplyTo is the id of the thread root this reply belongs to, or 0 when
	// this comment is itself a root.
	//
	// Threads are one level deep. Replying to a reply joins that reply's
	// thread rather than starting a branch, because a discussion that can
	// fork is a discussion "is this settled" cannot be asked of.
	InReplyTo int64

	AuthorID int64
	Body     string

	// ResolvedAt is the zero time while the thread is open, and ResolvedBy is
	// who closed it. Only a root carries them; a reply is resolved by the
	// thread it belongs to.
	ResolvedAt time.Time
	ResolvedBy int64

	CreatedAt time.Time
}

// IsReply reports whether this comment hangs off a thread root.
func (c Comment) IsReply() bool { return c.InReplyTo != 0 }

// IsRoot reports whether this comment is a thread of its own.
func (c Comment) IsRoot() bool { return c.InReplyTo == 0 }

// Resolved reports whether the thread has been closed.
func (c Comment) Resolved() bool { return !c.ResolvedAt.IsZero() }

// Thread is one root comment and its replies, oldest first.
type Thread struct {
	Comment

	Replies []Comment
}

// Open reports whether the thread is still an unanswered question, which is
// exactly what GuardCommentsResolved counts.
func (t Thread) Open() bool { return !t.Resolved() }

// Threads groups comments into threads, oldest thread first and each thread's
// replies oldest first.
//
// It is a pure function over rows the store loaded in one query, rather than a
// query per thread: a document with forty comments would otherwise be
// forty-one reads to draw one page.
//
// A reply whose root is not in the input is promoted to a thread of its own.
// That cannot happen through this system -- comments are loaded a whole
// document at a time and in_reply_to is a foreign key into the same table --
// and the alternative is dropping a comment somebody wrote, which is a worse
// answer to an impossible case than showing it in the wrong place.
func Threads(comments []Comment) []Thread {
	roots := make([]Thread, 0, len(comments))
	at := map[int64]int{}
	for _, c := range comments {
		if c.IsRoot() {
			at[c.ID] = len(roots)
			roots = append(roots, Thread{Comment: c})
		}
	}
	var orphans []Comment
	for _, c := range comments {
		if c.IsRoot() {
			continue
		}
		i, ok := at[c.InReplyTo]
		if !ok {
			orphans = append(orphans, c)
			continue
		}
		roots[i].Replies = append(roots[i].Replies, c)
	}
	for _, c := range orphans {
		roots = append(roots, Thread{Comment: c})
	}

	slices.SortStableFunc(roots, func(a, b Thread) int { return compareInt64(a.ID, b.ID) })
	for i := range roots {
		slices.SortStableFunc(roots[i].Replies, func(a, b Comment) int { return compareInt64(a.ID, b.ID) })
	}
	return roots
}

// OpenThreads counts the threads that are still open. It is what a refusal
// naming the open thread count reports (PLAN.md M11 acceptance 5).
func OpenThreads(threads []Thread) int {
	n := 0
	for _, t := range threads {
		if t.Open() {
			n++
		}
	}
	return n
}

// ValidateCommentBody reports whether a body is one this system will store,
// and returns the body it would store.
//
// Surrounding whitespace is trimmed, because a comment that is one newline is
// an empty comment with a character in it. Everything else is kept verbatim:
// a comment is prose, and a system that reformats what somebody wrote about a
// document is a system people stop writing in.
func ValidateCommentBody(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("body: a comment with nothing in it is not a comment: %w", ErrInvalid)
	}
	if n := utf8.RuneCountInString(trimmed); n > MaxCommentBody {
		return "", fmt.Errorf("body: %d characters, and the limit is %d: %w", n, MaxCommentBody, ErrInvalid)
	}
	return trimmed, nil
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
