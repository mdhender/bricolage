// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"fmt"
	"net"
	"time"
)

// DefaultAddr is the address cmsd binds when nobody says otherwise.
//
// It is a loopback address, never ":8080" and never "0.0.0.0:port". cmsd
// speaks plain HTTP and never terminates TLS (invariant 15); a reverse proxy
// does that and reaches it here. Binding a TLS-less server to a public
// interface is the kind of mistake that survives to production, so the default
// cannot make it. Someone who genuinely needs another interface passes --addr
// explicitly and owns the consequence.
const DefaultAddr = "127.0.0.1:18443"

// DefaultTimeout is the value of --timeout when it is not given: no timeout.
//
// The flag is ungated and ships in production builds, because it is not
// dangerous — the worst it does is stop a server that its own operator
// configured to stop (DESIGN.md 11).
const DefaultTimeout time.Duration = 0

// DefaultDrainTimeout bounds the graceful shutdown itself: after this long,
// in-flight requests that have not finished are abandoned rather than holding
// the process open. It is not configurable until something needs it to be.
const DefaultDrainTimeout = 30 * time.Second

// AddrIsLoopback reports whether addr names a loopback interface. It is false
// for a bare ":port" and for a wildcard host, both of which bind every
// interface on the machine.
//
// This is a question about a string, not a policy: --addr may be anything the
// operator asks for. It exists so that a test can assert what DefaultAddr is,
// and so that cmsd can say plainly in its log that it bound something public.
func AddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// A name rather than a literal. Resolve nothing here; treat only the two
	// spellings that cannot mean anything else as loopback.
	return host == "localhost"
}

// ValidateAddr reports whether addr is a usable "host:port" for a listener.
//
// It deliberately permits a bare ":port" and a wildcard host. The design's rule
// is about the default, not about what an operator may ask for: someone who
// genuinely needs a public interface passes it explicitly (DESIGN.md 11). What
// cmsd owes them in return is a loud line in the log saying so, not a refusal.
func ValidateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("addr %q: no port", addr)
	}
	return nil
}
