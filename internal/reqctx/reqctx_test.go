// Copyright (c) 2026 Michael D Henderson.

package reqctx

import (
	"context"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// TestZeroValues: every accessor answers for a context nothing wrote to, and
// the answers are distinguishable from a real value.
//
// A handler reached in a test through httptest without the middleware has no
// resolved address, and inventing one here would hide the wiring mistake that
// produced it.
func TestZeroValues(t *testing.T) {
	ctx := context.Background()

	if got := ClientAddr(ctx); got != "" {
		t.Errorf("ClientAddr = %q, want empty", got)
	}
	if got := RequestID(ctx); got != "" {
		t.Errorf("RequestID = %q, want empty", got)
	}
	if _, ok := Identity(ctx); ok {
		t.Error("Identity reported a caller on an empty context")
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	ctx = WithClientAddr(ctx, "203.0.113.5")
	ctx = WithRequestID(ctx, "abc123")
	ctx = WithIdentity(ctx, domain.Identity{User: domain.User{Email: "admin@example.com"}})

	if got := ClientAddr(ctx); got != "203.0.113.5" {
		t.Errorf("ClientAddr = %q", got)
	}
	if got := RequestID(ctx); got != "abc123" {
		t.Errorf("RequestID = %q", got)
	}
	id, ok := Identity(ctx)
	if !ok {
		t.Fatal("Identity reported no caller")
	}
	if id.User.Email != "admin@example.com" {
		t.Errorf("Identity = %+v", id.User)
	}
}

// TestKeysAreUnexported is the property that makes the accessors the only way
// in: a context key that is a string can be written by anything that guesses
// the string, and an identity somebody else can write is not an identity.
func TestKeysAreUnexported(t *testing.T) {
	ctx := context.WithValue(context.Background(), "clientAddr", "203.0.113.5") //nolint:staticcheck // the point of the test
	if got := ClientAddr(ctx); got != "" {
		t.Errorf("a string key reached ClientAddr, returning %q", got)
	}
}
