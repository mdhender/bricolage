// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"strings"
	"testing"
)

// The related-asset cascade's policy (DESIGN.md 8.2, PLAN.md M10).

func TestParseRelatedFailure(t *testing.T) {
	tests := []struct {
		in      string
		want    RelatedFailure
		wantErr bool
	}{
		{"fail", RelatedFailureFail, false},
		{"warn", RelatedFailureWarn, false},
		{"", DefaultRelatedFailure, false},

		// A misspelling is refused rather than read as the default. A config
		// file that says "warm" and a system that behaves as "fail" is the
		// shape invariant 6 is about, one setting smaller.
		{"warm", DefaultRelatedFailure, true},
		{"Warn", DefaultRelatedFailure, true},
		{" warn", DefaultRelatedFailure, true},
		{"true", DefaultRelatedFailure, true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRelatedFailure(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("ParseRelatedFailure(%q) accepted it", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ParseRelatedFailure(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRelatedFailure(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRelatedFailureRefusalNamesTheKey keeps the flag, the config key, and the
// message saying the same words. Somebody reading "publish.related_failure" in
// an error goes to the right line of DESIGN.md 8.2.
func TestRelatedFailureRefusalNamesTheKey(t *testing.T) {
	_, err := ParseRelatedFailure("nope")
	if err == nil {
		t.Fatal("ParseRelatedFailure accepted an unrecognised word")
	}
	for _, want := range []string{"publish.related_failure", "fail", "warn", "nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, missing %q", err, want)
		}
	}
}

// TestDefaultRelatedFailureIsFailSafe is the same assertion the environment's
// default gets: the direction a value nobody set takes must be the safe one.
// A publish that quietly left a referenced document behind puts a page live
// linking to something that is not there, and the reader finds out.
func TestDefaultRelatedFailureIsFailSafe(t *testing.T) {
	if DefaultRelatedFailure != RelatedFailureFail {
		t.Errorf("DefaultRelatedFailure = %q, want %q", DefaultRelatedFailure, RelatedFailureFail)
	}
	if !DefaultRelatedFailure.Aborts() {
		t.Error("the default policy does not abort on a refusal")
	}
	if RelatedFailureWarn.Aborts() {
		t.Error("warn aborts; it is the policy that does not")
	}
}
