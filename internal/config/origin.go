// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// DefaultPublicOrigin is the origin a browser reaches this system at in
// development: the Caddy service's TLS listener, not the Go one
// (DESIGN.md 11).
//
// The public origin is configuration, never inference. cmsd is told its
// external origin and never derives it from the Host header, because absolute
// URLs, cookie attributes, and the CSRF trusted-origin list all come from this
// value and a header is attacker-controlled.
const DefaultPublicOrigin = "https://htmx-app.localhost:8443"

// DefaultSessionTTL is how long a session lasts. It is long enough not to
// interrupt a day's work and short enough that a token left in a shell history
// stops working.
const DefaultSessionTTL = 12 * time.Hour

// SessionCookieName is the cookie the HTMX UI authenticates with. It is
// prefixed "__Host-" so the browser enforces what DESIGN.md 14 requires of us:
// the prefix is only accepted on a cookie that is Secure, has no Domain, and
// has Path=/, which makes a cookie without Secure not merely wrong but
// rejected.
const SessionCookieName = "__Host-cms_session"

// DefaultTrustedProxies is the CIDR list X-Forwarded-* headers are honoured
// from: loopback, which is where the reverse proxy is (DESIGN.md 11).
//
// Anywhere else the headers are attacker-controlled, and a client address
// resolved from an attacker-controlled header is one that lands in the event
// log and the rate limiter (invariant 14).
var DefaultTrustedProxies = []string{"127.0.0.1/32", "::1/128"}

// ParseTrustedProxies turns the CIDR list into matchers.
//
// A bare address is accepted and read as a single-host range, because
// "127.0.0.1" is what somebody writes when they mean "127.0.0.1/32" and
// refusing it teaches nothing.
func ParseTrustedProxies(cidrs []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(c)
		if ip == nil {
			return nil, fmt.Errorf("trusted proxy %q: not an address or a CIDR block", c)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// PublicOrigin is a validated external origin: scheme and host, no path.
type PublicOrigin struct {
	// URL is the parsed origin. Scheme and Host are set; nothing else is.
	URL *url.URL
}

// ParsePublicOrigin validates and normalises an origin.
//
// It must be absolute, carry a host, and carry no path, query, or fragment. An
// origin with a path is the mistake that produces "https://host/app/#/login"
// in a redirect and a cookie scoped to the wrong place.
func ParsePublicOrigin(s string) (PublicOrigin, error) {
	if s == "" {
		return PublicOrigin{}, fmt.Errorf("public origin: required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return PublicOrigin{}, fmt.Errorf("public origin %q: %w", s, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return PublicOrigin{}, fmt.Errorf("public origin %q: scheme must be http or https", s)
	case u.Host == "":
		return PublicOrigin{}, fmt.Errorf("public origin %q: no host", s)
	case u.Path != "" && u.Path != "/":
		return PublicOrigin{}, fmt.Errorf("public origin %q: an origin has no path", s)
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		return PublicOrigin{}, fmt.Errorf("public origin %q: an origin has no query, fragment, or userinfo", s)
	}
	return PublicOrigin{URL: &url.URL{Scheme: u.Scheme, Host: u.Host}}, nil
}

// String renders the origin: scheme, "://", host. No trailing slash.
func (o PublicOrigin) String() string {
	if o.URL == nil {
		return ""
	}
	return o.URL.Scheme + "://" + o.URL.Host
}

// ValidateReturnTo checks a returnTo parameter against this origin and returns
// the URL to redirect to (DESIGN.md 11).
//
// The rule is that it must be a relative path, or absolute with an origin
// equal to this one. Everything else is a refusal, and the refusals matter
// more than the acceptances:
//
//   - "//evil.example.com" is a scheme-relative URL. It looks like a path, it
//     starts with a slash, and a browser reads it as another origin.
//   - "https://evil.example.com@localhost" puts the attacker's host in the
//     userinfo, where a careless parse reads it as the host and a browser does
//     not.
//   - "/\evil.example.com" and "\\evil.example.com" are read as scheme-relative
//     by some browsers, because a backslash is normalised to a slash.
//
// An open redirect in a route that only exists in development is still an open
// redirect, and this is the pattern that gets copied into the route that does
// not.
func (o PublicOrigin) ValidateReturnTo(returnTo string) (string, error) {
	if returnTo == "" {
		return "", fmt.Errorf("returnTo: empty")
	}
	if strings.ContainsAny(returnTo, "\\\r\n\t") {
		return "", fmt.Errorf("returnTo %q: a URL contains no backslash or control character", returnTo)
	}

	u, err := url.Parse(returnTo)
	if err != nil {
		return "", fmt.Errorf("returnTo %q: %w", returnTo, err)
	}
	if u.User != nil {
		return "", fmt.Errorf("returnTo %q: userinfo in a redirect target names one host to us and another to the browser", returnTo)
	}

	// Relative: no scheme and no authority. "//host/path" parses with an
	// empty scheme and a non-empty Host, which is why Host is checked and not
	// just Scheme.
	if u.Scheme == "" && u.Host == "" {
		if !strings.HasPrefix(u.Path, "/") {
			return "", fmt.Errorf("returnTo %q: a relative target must begin with \"/\"", returnTo)
		}
		return u.String(), nil
	}

	if u.Scheme != o.URL.Scheme || u.Host != o.URL.Host {
		return "", fmt.Errorf("returnTo %q: not %s", returnTo, o)
	}
	return u.String(), nil
}
