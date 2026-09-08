// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"net"
	"testing"
)

// TestDefaultAddrIsLoopback is PLAN.md M0 acceptance 6. cmsd speaks plain HTTP
// and never terminates TLS (invariant 15), so a default that binds a public
// interface ships an unencrypted server to production. The default has to be
// loopback and a test has to say so, because this is the kind of change that
// looks harmless in a diff.
func TestDefaultAddrIsLoopback(t *testing.T) {
	host, port, err := net.SplitHostPort(DefaultAddr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q) = %v", DefaultAddr, err)
	}
	if host == "" {
		t.Fatalf("DefaultAddr = %q: bare %q binds every interface", DefaultAddr, ":"+port)
	}
	if port == "" {
		t.Fatalf("DefaultAddr = %q: no port", DefaultAddr)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("DefaultAddr host %q is not an IP literal; resolution is not a default's business", host)
	}
	if ip.IsUnspecified() {
		t.Fatalf("DefaultAddr = %q: %q is the wildcard address", DefaultAddr, host)
	}
	if !ip.IsLoopback() {
		t.Fatalf("DefaultAddr = %q: %q is not loopback", DefaultAddr, host)
	}
	if !AddrIsLoopback(DefaultAddr) {
		t.Fatalf("AddrIsLoopback(%q) = false", DefaultAddr)
	}
}

func TestAddrIsLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:18443", true},
		{"127.0.0.2:18443", true},
		{"[::1]:18443", true},
		{"localhost:18443", true},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{":8080", false},
		{"192.168.1.10:8080", false},
		{"example.com:8080", false},
		{"garbage", false},
		{"", false},
	} {
		if got := AddrIsLoopback(tc.addr); got != tc.want {
			t.Errorf("AddrIsLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestValidateAddrPermitsExplicitPublicBinds records a deliberate decision.
// The design's rule is about the default, not about what an operator may ask
// for: someone who genuinely needs a public interface passes it explicitly
// (DESIGN.md 11). Refusing it here would only teach them to patch the check.
func TestValidateAddrPermitsExplicitPublicBinds(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:18443", "0.0.0.0:8080", ":8080", "[::]:80"} {
		if err := ValidateAddr(addr); err != nil {
			t.Errorf("ValidateAddr(%q) = %v, want nil", addr, err)
		}
	}
}

func TestValidateAddrRejectsUnusable(t *testing.T) {
	for _, addr := range []string{"", "127.0.0.1", "127.0.0.1:", "no colons here"} {
		if err := ValidateAddr(addr); err == nil {
			t.Errorf("ValidateAddr(%q) = nil, want an error", addr)
		}
	}
}

// TestDefaultTimeoutIsNever pins the other fail-safe default: a server that
// stops after a duration nobody asked for is a worse surprise than one that
// runs until it is told to stop.
func TestDefaultTimeoutIsNever(t *testing.T) {
	if DefaultTimeout != 0 {
		t.Fatalf("DefaultTimeout = %v, want 0 (never)", DefaultTimeout)
	}
}
