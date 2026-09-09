// Copyright (c) 2026 Michael D Henderson.

package config

import "fmt"

// The related-asset cascade's policy (DESIGN.md 8.2, PLAN.md M10).
//
// Publishing a document publishes the documents it references, and some of
// them cannot be published: the caller may not, the workflow does not call
// their state publishable, somebody is editing one, or one has never been
// checked in. What to do about that is a policy and not a rule, because the
// two answers are both defensible and an installation knows which it wants.
//
// It is configuration rather than schema for the reason a saved queue is
// (DESIGN.md 14): it has no identity anybody refers to, nothing points at it,
// and an installation that wants the other answer wants a different config
// file rather than a migration and an admin screen.
//
// This package names it in its own vocabulary, as two words, because config
// imports the standard library and nothing else (doc.go). internal/service is
// where the word becomes a decision about a set of documents.

// RelatedFailure is what a publish does when a document it would have to
// publish cannot be published.
type RelatedFailure string

const (
	// RelatedFailureFail publishes nothing at all. The refusals are reported
	// and the root stays where it was.
	RelatedFailureFail RelatedFailure = "fail"

	// RelatedFailureWarn publishes the root and everything that could be
	// gathered, and reports the refusals.
	RelatedFailureWarn RelatedFailure = "warn"
)

// RelatedFailures are the two answers, in a stable order for the flag's help
// text and for the tests.
var RelatedFailures = []RelatedFailure{RelatedFailureFail, RelatedFailureWarn}

// DefaultRelatedFailure is "fail", which is the fail-safe direction and the
// same direction the environment's default takes (DESIGN.md 14).
//
// A publish that quietly left a referenced document behind puts a page live
// with a link to something that is not there, and the person who finds out is
// a reader. A publish that refuses puts the decision in front of the editor,
// who can then say "warn" and mean it.
const DefaultRelatedFailure = RelatedFailureFail

// ParseRelatedFailure maps a configured word to a policy.
//
// An unrecognised word is refused rather than read as the default, for the
// reason ParseQueueAssignee refuses one: a misspelled "warn" silently read as
// "fail" is a configuration file that says one thing while the system does
// another, and nobody would find out until a publish they expected refused.
//
// The empty string is the default and not an error: it is what "the config
// file did not mention this" looks like, and requiring every installation to
// spell out a setting it has no opinion about is how a config file becomes
// unreadable.
func ParseRelatedFailure(s string) (RelatedFailure, error) {
	switch RelatedFailure(s) {
	case "":
		return DefaultRelatedFailure, nil
	case RelatedFailureFail:
		return RelatedFailureFail, nil
	case RelatedFailureWarn:
		return RelatedFailureWarn, nil
	default:
		return DefaultRelatedFailure, fmt.Errorf(
			"publish.related_failure %q: want %q or %q", s, RelatedFailureFail, RelatedFailureWarn)
	}
}

// Aborts reports whether a refusal stops the whole publish.
func (f RelatedFailure) Aborts() bool { return f != RelatedFailureWarn }

// String makes a policy printable in the startup banner and in logs.
func (f RelatedFailure) String() string { return string(f) }
