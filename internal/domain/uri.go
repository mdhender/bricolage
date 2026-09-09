// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strings"
	"time"
)

// URI construction (DESIGN.md 5.3, PLAN.md M7).
//
// A document's address is a pure function of four things: the document, the
// version being published, the category it is filed in, and the output channel
// it is going to. Nothing here reads a clock, a database, or a request, which
// is what lets the whole of it be a table of golden vectors.
//
// The one rule with teeth is the category substitution. Category paths already
// end in '/', so "%{categories}/%Y" would produce "/features//2026" if the
// token were replaced naively. The substitution therefore consumes the slash
// that follows it, and a property test over random category depths asserts
// that no URI ever contains "//" (PLAN.md M7 acceptance 4).

// The braced extensions to strftime (DESIGN.md 5.3).
const (
	// TokenCategories expands to the primary category's materialised path,
	// which begins and ends with '/'.
	TokenCategories = "%{categories}"

	// TokenSlug expands to the version's slug, or to nothing when the output
	// channel does not use one.
	TokenSlug = "%{slug}"
)

// The URI case rules an output channel may declare.
const (
	URICaseMixed = "mixed"
	URICaseLower = "lower"
	URICaseUpper = "upper"
)

// URICases are the rules, in a stable order for the CLI and the admin screens.
var URICases = []string{URICaseMixed, URICaseLower, URICaseUpper}

// ValidURICase reports whether s is one of the three.
func ValidURICase(s string) bool {
	for _, c := range URICases {
		if c == s {
			return true
		}
	}
	return false
}

// SubjectOutputChannel is the events subject kind for an output channel.
const SubjectOutputChannel = "output_channel"

// SubjectElementType is the events subject kind for an element type.
const SubjectElementType = "element_type"

// OutputChannel is where content goes and what its address looks like
// (DESIGN.md 5.3).
type OutputChannel struct {
	ID     int64
	UID    string
	SiteID int64
	Name   string

	// Protocol prefixes the absolute URL. It is not part of the URI: the URI
	// is a path, and the protocol and the site's domain make it a URL.
	Protocol string

	// Filename and FileExt name the file a URI's directory holds.
	Filename string
	FileExt  string

	// URIFormat is the strftime string with the %{categories} and %{slug}
	// extensions. FixedURIFormat is the one used for a document whose element
	// type sets fixed_uri: a page that lives at one address forever rather
	// than at one derived from the date it was published.
	URIFormat      string
	FixedURIFormat string

	// UseSlug decides whether %{slug} expands to anything.
	UseSlug bool

	// URICase is "mixed", "lower", or "upper".
	URICase string
}

// Validate reports whether an output channel is one the system will store.
func (oc OutputChannel) Validate() error {
	if oc.SiteID == 0 {
		return fmt.Errorf("output channel: no site: %w", ErrInvalid)
	}
	if strings.TrimSpace(oc.Name) == "" {
		return fmt.Errorf("output channel: a name is required: %w", ErrInvalid)
	}
	if strings.TrimSpace(oc.URIFormat) == "" {
		return fmt.Errorf("output channel %q: a uri_format is required: %w", oc.Name, ErrInvalid)
	}
	if strings.TrimSpace(oc.FixedURIFormat) == "" {
		return fmt.Errorf("output channel %q: a fixed_uri_format is required: %w", oc.Name, ErrInvalid)
	}
	if !ValidURICase(oc.URICase) {
		return fmt.Errorf("output channel %q: uri_case %q is not one of mixed, lower, upper: %w",
			oc.Name, oc.URICase, ErrInvalid)
	}
	if strings.TrimSpace(oc.Filename) == "" {
		return fmt.Errorf("output channel %q: a filename is required: %w", oc.Name, ErrInvalid)
	}
	// Both formats are compiled against a fixed instant, so a format naming a
	// conversion this system does not implement is refused when it is written
	// rather than when a document is published against it. A guard nobody can
	// fail at configuration time is a guard that fails at three in the
	// morning instead.
	probe := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, f := range []struct{ what, format string }{
		{"uri_format", oc.URIFormat},
		{"fixed_uri_format", oc.FixedURIFormat},
	} {
		if _, err := Strftime(stripBracedTokens(f.format), probe); err != nil {
			return fmt.Errorf("output channel %q: %s: %w", oc.Name, f.what, err)
		}
	}
	return nil
}

// stripBracedTokens removes the braced extensions so that the remainder can be
// handed to the strftime formatter, which knows nothing about them.
func stripBracedTokens(format string) string {
	format = strings.ReplaceAll(format, TokenCategories, "")
	return strings.ReplaceAll(format, TokenSlug, "")
}

// DefaultURIFormat and DefaultFixedURIFormat are what an output channel gets
// when nobody says otherwise.
//
// The dated one is the shape a newsroom expects and the shape the system we
// learned from shipped: the section, then the date, then the story. The fixed
// one has no date in it, which is the whole point of a fixed URI -- a page
// that stays where it is put.
const (
	DefaultURIFormat      = TokenCategories + "/%Y/%m/%d/" + TokenSlug
	DefaultFixedURIFormat = TokenCategories + "/" + TokenSlug
	DefaultFilename       = "index"
	DefaultFileExt        = "html"
	DefaultProtocol       = "https://"
)

// ParseCoverDate reads a version's cover date.
//
// The column is text and has been since 0004, holding whatever a client sent:
// "2026-03-01" from earl, an RFC 3339 instant from a richer client, and the
// millisecond form this system writes elsewhere. All three parse here, and
// anything else is an error naming the value rather than a zero time that
// looks like the epoch.
func ParseCoverDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("cover date: empty: %w", ErrInvalid)
	}
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02T15:04:05.000Z",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cover date %q: not a date this system recognises: %w", s, ErrInvalid)
}

// BuildURI computes a document's address in one output channel
// (PLAN.md M7 acceptance 3).
//
// It is pure. The format comes from the output channel -- the fixed one when
// the document's element type declares a fixed URI -- the categories token
// expands to the category's materialised path, the slug token expands to the
// version's slug when the channel uses one, and everything else is strftime
// against the cover date.
//
// A format that names a date conversion and a version with no cover date is a
// refusal rather than a URI with a hole in it. That case is real and it is not
// an error in itself: a fixed-URI page usually has no cover date, and its
// format has no date conversion, so it builds. What is refused is the
// combination.
func BuildURI(doc Document, ver Version, cat Category, oc OutputChannel) (string, error) {
	format := oc.URIFormat
	if doc.ElementTypeFixedURI {
		format = oc.FixedURIFormat
	}
	if strings.TrimSpace(format) == "" {
		return "", fmt.Errorf("output channel %q has no URI format: %w", oc.Name, ErrInvalid)
	}
	if err := ValidatePath(cat.Path); err != nil {
		return "", err
	}

	slug := ""
	if oc.UseSlug {
		slug = ver.Slug
		if err := validateSlugForURI(slug); err != nil {
			return "", err
		}
	}

	// The braced extensions are substituted before the date conversions run,
	// because one of them consumes the slash that follows it and that is a URI
	// rule rather than a date rule.
	expanded := substituteToken(format, TokenCategories, cat.Path)
	expanded = substituteToken(expanded, TokenSlug, slug)

	var (
		at  time.Time
		err error
	)
	if HasDateConversion(expanded) {
		if ver.CoverDate == "" {
			return "", fmt.Errorf(
				"output channel %q builds a URI from the cover date and this version has none: %w",
				oc.Name, ErrInvalid)
		}
		if at, err = ParseCoverDate(ver.CoverDate); err != nil {
			return "", err
		}
	}
	expanded, err = Strftime(expanded, at)
	if err != nil {
		return "", fmt.Errorf("output channel %q: %w", oc.Name, err)
	}
	return NormalizeURI(expanded, oc.URICase), nil
}

// FileURI is the file a URI's directory holds: the URI, the channel's
// filename, and its extension.
//
// It is separate from BuildURI because the two answer different questions.
// BuildURI answers "what address does this document have", which is what a
// link points at and what a stale-expiry diff compares; this answers "what
// file did we write", which is what internal/publish will record in
// published_resources.
func (oc OutputChannel) FileURI(uri string) string {
	name := oc.Filename
	if name == "" {
		name = DefaultFilename
	}
	ext := oc.FileExt
	if ext == "" {
		ext = DefaultFileExt
	}
	if uri == "/" {
		return "/" + name + "." + ext
	}
	return uri + "/" + name + "." + ext
}

// URL renders an absolute address for a URI on a site's domain.
func (oc OutputChannel) URL(domainName, uri string) string {
	protocol := oc.Protocol
	if protocol == "" {
		protocol = DefaultProtocol
	}
	return protocol + domainName + uri
}

// substituteToken replaces every occurrence of token with value, consuming the
// slash that follows the token when the substitution would otherwise produce
// an empty segment.
//
// That is the rule DESIGN.md 5.3 states for the category token -- "the
// category segment substitution deliberately consumes the following slash,
// because category paths already end in one" -- generalised to the condition
// that makes it correct: consume when the value ends in '/', because it
// already carries the separator, and when the value is empty, because there is
// nothing to separate. A slug that has a value keeps the slash that follows
// it, since joining it to the next segment would be a different address.
func substituteToken(format, token, value string) string {
	var b strings.Builder
	b.Grow(len(format) + len(value))

	for {
		i := strings.Index(format, token)
		if i < 0 {
			b.WriteString(format)
			return b.String()
		}
		b.WriteString(format[:i])
		b.WriteString(value)
		format = format[i+len(token):]
		if (value == "" || strings.HasSuffix(value, "/")) && strings.HasPrefix(format, "/") {
			format = format[1:]
		}
	}
}

// NormalizeURI renders a built URI the way it is stored and served: absolute,
// with no empty segment, with no trailing slash except at the root, and in the
// channel's case.
//
// The empty-segment removal is a second line of defence rather than the rule.
// The rule is that the category token consumes the following slash, and a
// property test asserts it over random category depths; this makes a format
// with a typo in it -- "%{categories}//%Y" -- produce an address rather than
// one nobody can fetch.
func NormalizeURI(uri, uriCase string) string {
	segments := make([]string, 0, 8)
	for _, s := range strings.Split(uri, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	out := "/" + strings.Join(segments, "/")

	switch uriCase {
	case URICaseLower:
		out = strings.ToLower(out)
	case URICaseUpper:
		out = strings.ToUpper(out)
	}
	return out
}

// validateSlugForURI refuses a slug that would change what the address means.
//
// It is not a general slug rule and it is deliberately not applied on edit: a
// working draft may hold anything, and refusing a slug at the moment somebody
// types it is how a writer loses a sentence. What is refused here are the four
// characters that make a URI a different URI -- a separator, a query, a
// fragment, and a space -- because a slug containing one of them does not
// produce a bad address, it produces somebody else's.
func validateSlugForURI(slug string) error {
	for _, r := range slug {
		switch {
		case r == '/', r == '?', r == '#':
			return fmt.Errorf("slug %q: %q makes it a different address: %w", slug, r, ErrInvalid)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			return fmt.Errorf("slug %q: whitespace is not allowed in a URI segment: %w", slug, ErrInvalid)
		}
	}
	return nil
}
