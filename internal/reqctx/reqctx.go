// Copyright (c) 2026 Michael D Henderson.

// Package reqctx carries the per-request values that more than one transport
// needs: the resolved client address, the request id, and the authenticated
// identity.
//
// It exists because these are resolved once, in middleware, and read from the
// context afterwards (DESIGN.md 14). The client address in particular must be
// resolved exactly once: X-Forwarded-For is honoured only from a configured
// trusted proxy (invariant 14), and a second parse somewhere downstream is a
// second policy that can disagree with the first.
//
// It is a leaf. internal/api, internal/web, internal/web/devroutes and
// internal/server all read from it, and it imports nothing but the standard
// library and internal/domain, so none of them has to import another.
//
// Permitted imports: the standard library, internal/domain.
package reqctx

import (
	"context"

	"github.com/mdhender/bricolage/internal/domain"
)

// key is the unexported context key type, so nothing outside this package can
// write these values by colliding with a string.
type key int

const (
	keyClientAddr key = iota
	keyRequestID
	keyIdentity
)

// WithClientAddr returns a context carrying the resolved client address.
//
// Only the middleware that owns the trusted-proxy policy calls this. Handlers
// read with ClientAddr.
func WithClientAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, keyClientAddr, addr)
}

// ClientAddr returns the resolved client address, or the empty string when no
// middleware resolved one.
//
// The empty string is a real answer and callers must handle it: a handler
// reached in a test through httptest without the middleware has no resolved
// address, and inventing one here would hide the wiring mistake that produced
// it.
func ClientAddr(ctx context.Context) string {
	s, _ := ctx.Value(keyClientAddr).(string)
	return s
}

// WithRequestID returns a context carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID returns the request id, or the empty string.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

// WithIdentity returns a context carrying the authenticated caller: the user,
// the roles they hold, and the grants those roles carry.
func WithIdentity(ctx context.Context, id domain.Identity) context.Context {
	return context.WithValue(ctx, keyIdentity, id)
}

// Identity returns the authenticated caller and whether there is one.
//
// There is deliberately no variant that returns a zero Identity without
// saying so. A zero Identity has no grants, which resolves to no privilege
// everywhere -- safe, but indistinguishable from a real user who has been
// granted nothing, and the two want different answers at the edge: 401 for the
// first and 403 for the second.
func Identity(ctx context.Context) (domain.Identity, bool) {
	id, ok := ctx.Value(keyIdentity).(domain.Identity)
	return id, ok
}
