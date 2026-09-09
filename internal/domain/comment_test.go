// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// domain is pure, so these are table-driven and exhaustive (DESIGN.md 15).
// Threads is the one piece of real arithmetic M11 puts here: the store loads a
// document's comments in one query and this is what turns them into a
// discussion.

func TestThreads(t *testing.T) {
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	comment := func(id, replyTo int64) Comment {
		return Comment{ID: id, UID: "c" + string(rune('0'+id)), InReplyTo: replyTo, CreatedAt: at}
	}

	for _, tc := range []struct {
		name     string
		in       []Comment
		wantRoot []int64
		wantRepl map[int64][]int64
	}{
		{
			name:     "no comments is no threads",
			in:       nil,
			wantRoot: nil,
		},
		{
			name:     "roots stay in id order",
			in:       []Comment{comment(2, 0), comment(1, 0)},
			wantRoot: []int64{1, 2},
		},
		{
			name:     "replies hang off their root, oldest first",
			in:       []Comment{comment(1, 0), comment(3, 1), comment(2, 1)},
			wantRoot: []int64{1},
			wantRepl: map[int64][]int64{1: {2, 3}},
		},
		{
			name:     "two threads keep their own replies",
			in:       []Comment{comment(1, 0), comment(2, 0), comment(3, 1), comment(4, 2)},
			wantRoot: []int64{1, 2},
			wantRepl: map[int64][]int64{1: {3}, 2: {4}},
		},
		{
			// It cannot happen through this system -- comments are loaded a
			// whole document at a time and in_reply_to is a foreign key into
			// the same table -- and dropping something somebody wrote is a
			// worse answer to an impossible case than showing it.
			name:     "a reply whose root is missing is still shown",
			in:       []Comment{comment(2, 99)},
			wantRoot: []int64{2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Threads(tc.in)
			if len(got) != len(tc.wantRoot) {
				t.Fatalf("Threads returned %d threads, want %d", len(got), len(tc.wantRoot))
			}
			for i, want := range tc.wantRoot {
				if got[i].ID != want {
					t.Errorf("thread %d is comment %d, want %d", i, got[i].ID, want)
				}
				var ids []int64
				for _, r := range got[i].Replies {
					ids = append(ids, r.ID)
				}
				wantReplies := tc.wantRepl[want]
				if len(ids) != len(wantReplies) {
					t.Fatalf("thread %d has replies %v, want %v", want, ids, wantReplies)
				}
				for j := range ids {
					if ids[j] != wantReplies[j] {
						t.Errorf("thread %d reply %d is %d, want %d", want, j, ids[j], wantReplies[j])
					}
				}
			}
		})
	}
}

// TestOpenThreadsCountsRootsOnly is the number GuardCommentsResolved refuses
// on (PLAN.md M11 acceptance 5). A thread with three replies is one open
// question, not four.
func TestOpenThreadsCountsRootsOnly(t *testing.T) {
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	threads := Threads([]Comment{
		{ID: 1, CreatedAt: at},
		{ID: 2, InReplyTo: 1, CreatedAt: at},
		{ID: 3, InReplyTo: 1, CreatedAt: at},
		{ID: 4, CreatedAt: at, ResolvedAt: at, ResolvedBy: 7},
	})
	if n := OpenThreads(threads); n != 1 {
		t.Errorf("OpenThreads = %d, want 1: a resolved thread is closed and a reply is not a thread", n)
	}
	if !threads[0].Open() {
		t.Error("the unresolved thread reports itself closed")
	}
	if threads[1].Open() {
		t.Error("the resolved thread reports itself open")
	}
}

func TestValidateCommentBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		bad  bool
	}{
		{name: "ordinary", in: "the lede is buried", want: "the lede is buried"},
		{name: "surrounding whitespace goes", in: "  spaced  ", want: "spaced"},
		{name: "interior formatting stays", in: "one\n\ntwo", want: "one\n\ntwo"},
		{name: "empty", in: "", bad: true},
		{name: "only whitespace", in: " \n\t ", bad: true},
		{name: "too long", in: strings.Repeat("x", MaxCommentBody+1), bad: true},
		{name: "exactly the limit", in: strings.Repeat("x", MaxCommentBody), want: strings.Repeat("x", MaxCommentBody)},
		{name: "the limit is runes, not bytes", in: strings.Repeat("é", MaxCommentBody), want: strings.Repeat("é", MaxCommentBody)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateCommentBody(tc.in)
			if tc.bad {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("ValidateCommentBody(%q) = %q, %v, want ErrInvalid", tc.in, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateCommentBody: %v", err)
			}
			if got != tc.want {
				t.Errorf("ValidateCommentBody(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCommentShape(t *testing.T) {
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	root := Comment{ID: 1}
	reply := Comment{ID: 2, InReplyTo: 1}
	closed := Comment{ID: 3, ResolvedAt: at}

	if !root.IsRoot() || root.IsReply() || root.Resolved() {
		t.Error("a fresh root reports itself as a reply or as resolved")
	}
	if reply.IsRoot() || !reply.IsReply() {
		t.Error("a reply reports itself as a root")
	}
	if !closed.Resolved() {
		t.Error("a comment with a resolved_at reports itself open")
	}
}
