// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strings"
)

// Site is one publication (DESIGN.md 5.3).
//
// It is the outermost scope dimension of a grant and the domain an output
// channel's URLs resolve against. Every site has exactly one root category,
// created in the same transaction as the site itself, so that path arithmetic
// is total: a document filed nowhere in particular is filed at "/".
type Site struct {
	ID     int64
	UID    string
	Name   string
	Domain string
	Active bool
}

// SubjectSite is the events subject kind for a site.
const SubjectSite = "site"

// MaxSiteDomainLen is the longest domain a site may carry, which is RFC 1035's
// limit on a name in the wire format and therefore the longest name anything
// could ever resolve.
const MaxSiteDomainLen = 253

// MaxSiteNameLen is the longest display name a site may carry. It is a number
// rather than no limit because the name is a human label that ends up in a
// listing, and a value with no ceiling is a column somebody eventually pastes a
// document into.
const MaxSiteNameLen = 200

// NormalizeSiteDomain folds a site's domain and refuses one that could not be
// a host (issue #3).
//
// It is the one place a domain typed by a person is checked, because the value
// is load-bearing twice over. It is the first path segment of the template tree
//
//	<templates>/<site domain>/<category path>/<element type key>.gohtml
//
// which is what keeps two sites that both have a "/features/" apart
// (DESIGN.md 8.4), and it is the host half of every URL the site publishes,
// which OutputChannel.URL builds by concatenation. A value carrying a "/" would
// be two directories in the first use and a path in the second; a value that is
// "." or ".." would be neither.
//
// Case is folded for the reason NormalizeEmail folds it: hostnames are
// case-insensitive, sites.domain is UNIQUE as of migration 0014, and
// "Example.com" and "example.com" arriving as two sites would be two template
// trees for one publication. Folding on the way in is also what makes the
// stored value and the directory an operator creates agree, since a template
// tree on Linux is case-sensitive even though the name it is spelled from is
// not.
//
// A port is allowed. The development site is served through Caddy on 8443, and
// a URL built without the port would be an address that does not answer; the
// only thing this refuses about one is a value that is not a number in range.
func NormalizeSiteDomain(s string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(s))
	if d == "" {
		return "", fmt.Errorf("site domain: required: %w", ErrInvalid)
	}
	if len(d) > MaxSiteDomainLen {
		return "", fmt.Errorf("site domain %q: longer than %d characters: %w", d, MaxSiteDomainLen, ErrInvalid)
	}

	host := d
	if colon := strings.LastIndexByte(d, ':'); colon >= 0 {
		host = d[:colon]
		if err := validPort(d, d[colon+1:]); err != nil {
			return "", err
		}
	}
	if host == "" {
		return "", fmt.Errorf("site domain %q: a port with no host: %w", d, ErrInvalid)
	}
	for _, label := range strings.Split(host, ".") {
		if err := validHostLabel(d, label); err != nil {
			return "", err
		}
	}
	return d, nil
}

// MaxHostLabelLen is RFC 1035's limit on one label of a domain name.
const MaxHostLabelLen = 63

// validHostLabel checks one dot-separated label of a host.
//
// The set is the letters, the digits and the hyphen, which is the preferred
// name syntax and not the whole of what a resolver will carry. Refusing an
// underscore and a non-ASCII rune here is deliberate: the value becomes a
// directory name in the template tree, and a name that is legal in DNS but
// spelled differently by two filesystems is a template that is found on one
// machine and not on the next. An internationalised domain is written in its
// A-label form -- "xn--..." -- which this accepts.
func validHostLabel(full, label string) error {
	switch {
	case label == "":
		return fmt.Errorf("site domain %q: an empty label: %w", full, ErrInvalid)
	case len(label) > MaxHostLabelLen:
		return fmt.Errorf("site domain %q: the label %q is longer than %d characters: %w",
			full, label, MaxHostLabelLen, ErrInvalid)
	case label[0] == '-' || label[len(label)-1] == '-':
		return fmt.Errorf("site domain %q: the label %q begins or ends with %q: %w",
			full, label, "-", ErrInvalid)
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return fmt.Errorf("site domain %q: %q is not allowed in a host; use letters, digits and %q: %w",
				full, r, "-", ErrInvalid)
		}
	}
	return nil
}

// validPort checks the part after the last colon.
func validPort(full, port string) error {
	if port == "" {
		return fmt.Errorf("site domain %q: a colon with no port: %w", full, ErrInvalid)
	}
	n := 0
	for _, r := range port {
		if r < '0' || r > '9' {
			return fmt.Errorf("site domain %q: %q is not a port: %w", full, port, ErrInvalid)
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return fmt.Errorf("site domain %q: %q is not a port: %w", full, port, ErrInvalid)
		}
	}
	if n == 0 {
		return fmt.Errorf("site domain %q: %q is not a port: %w", full, port, ErrInvalid)
	}
	return nil
}

// ValidateSiteName checks a site's display name.
//
// It is weak on purpose -- present, not absurdly long, no control characters --
// because a publication's name is prose and the system does nothing with it but
// print it. What it is emphatically not is the domain: the name may be "The
// Daily Paper" and the domain may not.
func ValidateSiteName(s string) error {
	name := strings.TrimSpace(s)
	switch {
	case name == "":
		return fmt.Errorf("site name: required: %w", ErrInvalid)
	case len(name) > MaxSiteNameLen:
		return fmt.Errorf("site name %q: longer than %d characters: %w", name, MaxSiteNameLen, ErrInvalid)
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return fmt.Errorf("site name %q: contains a control character: %w", name, ErrInvalid)
		}
	}
	return nil
}
