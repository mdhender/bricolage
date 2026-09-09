// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"testing"
	"time"
)

var lockStart = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// TestLockIsALease is DESIGN.md 5.1: an expired lock is not a lock. It is the
// difference between an editor who closed their laptop and a document that
// needs an administrator.
func TestLockIsALease(t *testing.T) {
	expires := lockStart.Add(time.Hour)

	for _, tc := range []struct {
		name   string
		lock   Lock
		user   int64
		now    time.Time
		held   bool
		byUser bool
	}{
		{"unlocked", Lock{}, 7, lockStart, false, false},
		{"held by the asker", Lock{UserID: 7, ExpiresAt: expires}, 7, lockStart, true, true},
		{"held by somebody else", Lock{UserID: 8, ExpiresAt: expires}, 7, lockStart, true, false},
		{"expired", Lock{UserID: 7, ExpiresAt: expires}, 7, expires.Add(time.Second), false, false},
		{"expiring exactly now", Lock{UserID: 7, ExpiresAt: expires}, 7, expires, false, false},
		{"nobody is not a holder", Lock{UserID: 0, ExpiresAt: expires}, 0, lockStart, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lock.Held(tc.now); got != tc.held {
				t.Errorf("Held = %t, want %t", got, tc.held)
			}
			if got := tc.lock.IsHeldBy(tc.user, tc.now); got != tc.byUser {
				t.Errorf("IsHeldBy(%d) = %t, want %t", tc.user, got, tc.byUser)
			}
		})
	}
}

func TestValidDocKind(t *testing.T) {
	for _, k := range DocKinds {
		if !ValidDocKind(k) {
			t.Errorf("ValidDocKind(%q) = false", k)
		}
	}
	for _, k := range []string{"", "Story", "article", "stories"} {
		if ValidDocKind(k) {
			t.Errorf("ValidDocKind(%q) = true", k)
		}
	}
}

func TestNewDocumentValidate(t *testing.T) {
	ok := NewDocument{SiteID: 1, Kind: KindStory, ElementTypeKey: "story", Title: "A Title"}

	for _, tc := range []struct {
		name string
		in   NewDocument
		want error
	}{
		{"the minimum", ok, nil},
		{"content that is a JSON object", with(ok, func(n *NewDocument) { n.Content = `{"body":"hi"}` }), nil},
		{"no site", with(ok, func(n *NewDocument) { n.SiteID = 0 }), ErrInvalid},
		{"no kind", with(ok, func(n *NewDocument) { n.Kind = "" }), ErrInvalid},
		{"a kind that is not one", with(ok, func(n *NewDocument) { n.Kind = "article" }), ErrInvalid},
		{"no element type", with(ok, func(n *NewDocument) { n.ElementTypeKey = "" }), ErrInvalid},
		{"a title of spaces", with(ok, func(n *NewDocument) { n.Title = "   " }), ErrInvalid},
		{"content that is not JSON", with(ok, func(n *NewDocument) { n.Content = "not json" }), ErrInvalid},
		{"content that is a JSON array", with(ok, func(n *NewDocument) { n.Content = `[1,2]` }), ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func with(n NewDocument, fn func(*NewDocument)) NewDocument {
	fn(&n)
	return n
}

func TestDraftUpdateApply(t *testing.T) {
	base := Version{Title: "Old", Slug: "old", Content: `{"a":1}`}

	t.Run("an empty update changes nothing", func(t *testing.T) {
		u := DraftUpdate{}
		if !u.IsEmpty() {
			t.Fatal("IsEmpty = false")
		}
		got, err := u.Apply(base)
		if err != nil {
			t.Fatal(err)
		}
		if got != base {
			t.Errorf("Apply changed the version: %+v", got)
		}
	})

	t.Run("an empty string is a value, not an absence", func(t *testing.T) {
		empty := ""
		got, err := DraftUpdate{Slug: &empty}.Apply(base)
		if err != nil {
			t.Fatal(err)
		}
		if got.Slug != "" {
			t.Errorf("Slug = %q, want it cleared", got.Slug)
		}
		if got.Title != base.Title {
			t.Errorf("Title = %q, want it untouched", got.Title)
		}
	})

	t.Run("a title may not be cleared", func(t *testing.T) {
		empty := " "
		if _, err := (DraftUpdate{Title: &empty}).Apply(base); !errors.Is(err, ErrInvalid) {
			t.Errorf("Apply = %v, want ErrInvalid", err)
		}
	})

	t.Run("content must be a JSON object", func(t *testing.T) {
		bad := "nope"
		if _, err := (DraftUpdate{Content: &bad}).Apply(base); !errors.Is(err, ErrInvalid) {
			t.Errorf("Apply = %v, want ErrInvalid", err)
		}
	})

	t.Run("Apply does not mutate its input", func(t *testing.T) {
		title := "New"
		if _, err := (DraftUpdate{Title: &title}).Apply(base); err != nil {
			t.Fatal(err)
		}
		if base.Title != "Old" {
			t.Errorf("the input was mutated: %q", base.Title)
		}
	})
}

func TestNormalizeContent(t *testing.T) {
	for in, want := range map[string]string{"": "{}", "   ": "{}", `{"a":1}`: `{"a":1}`} {
		if got := NormalizeContent(in); got != want {
			t.Errorf("NormalizeContent(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVersionIsDraft is DESIGN.md 5.1: there is no version 0, and "never
// checked in" is the absence of a timestamp.
func TestVersionIsDraft(t *testing.T) {
	if !(Version{Number: 1}).IsDraft() {
		t.Error("a version with no checked_in_at is not reported as the draft")
	}
	if (Version{Number: 1, CheckedInAt: lockStart}).IsDraft() {
		t.Error("a checked-in version is reported as the draft")
	}
}

// TestDocumentSubject is what authorization is resolved against.
func TestDocumentSubject(t *testing.T) {
	d := Document{ID: 12, SiteID: 3, Kind: KindStory}
	want := Subject{SiteID: 3, DocKind: KindStory, DocumentID: 12}
	if got := d.Subject(); got != want {
		t.Errorf("Subject = %+v, want %+v", got, want)
	}
}
