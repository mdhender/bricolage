// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// domain is pure, so publishing's vocabulary is tested exhaustively and
// table-driven here (AGENTS.md, "Testing"). What matters most is the pin:
// a publish job that lost its version id would publish whatever the draft had
// become, which is the one thing invariant 8 promises cannot happen.

func TestPublishPayloadRoundTripsAndKeepsThePin(t *testing.T) {
	p := PublishPayload{DocumentID: 7, VersionID: 5, ChannelIDs: []int64{1, 2}, ActorID: 3}
	encoded, err := p.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	// The wire name is DESIGN.md 8.1's, and it is asserted rather than assumed
	// because a rename would silently orphan every scheduled job in a running
	// system.
	if !strings.Contains(encoded, `"document_version_id":5`) {
		t.Errorf("payload = %s, want document_version_id in it", encoded)
	}

	got, err := ParsePublishPayload(encoded)
	if err != nil {
		t.Fatalf("ParsePublishPayload: %v", err)
	}
	if got.VersionID != p.VersionID || got.DocumentID != p.DocumentID || got.ActorID != p.ActorID {
		t.Errorf("round trip = %+v, want %+v", got, p)
	}
	if len(got.ChannelIDs) != 2 {
		t.Errorf("channels = %v, want two", got.ChannelIDs)
	}
}

func TestPublishPayloadRefusesAnUnpinnedJob(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload PublishPayload
	}{
		{"no document", PublishPayload{VersionID: 5}},
		{"no version", PublishPayload{DocumentID: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.payload.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v, want invalid", err)
			}
		})
	}
	if _, err := ParsePublishPayload(`{"document_id":1}`); !errors.Is(err, ErrInvalid) {
		t.Errorf("a payload with no pin parsed: %v", err)
	}
	if _, err := ParsePublishPayload(`not json`); !errors.Is(err, ErrInvalid) {
		t.Errorf("a payload that is not JSON parsed: %v", err)
	}
}

func TestExpirePayloadNeedsAPath(t *testing.T) {
	if err := (ExpirePayload{DocumentID: 1}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("an expire job with nothing to delete validated")
	}
	p := ExpirePayload{DocumentID: 1, OutputChannelID: 2, URI: "/a/b", Path: "a/b/index.html"}
	encoded, err := p.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	got, err := ParseExpirePayload(encoded)
	if err != nil {
		t.Fatalf("ParseExpirePayload: %v", err)
	}
	if got != p {
		t.Errorf("round trip = %+v, want %+v", got, p)
	}
}

// TestJobPrioritiesAreDeliberate is not decoration. A publish somebody is
// waiting for must outrank the expiry of a file nobody can reach, or a slug
// change takes the old page down before the new one goes up.
func TestJobPrioritiesAreDeliberate(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	pub, err := PublishJob(PublishPayload{DocumentID: 1, VersionID: 2}, at)
	if err != nil {
		t.Fatalf("PublishJob: %v", err)
	}
	exp, err := ExpireJob(ExpirePayload{DocumentID: 1, Path: "a/index.html"}, at)
	if err != nil {
		t.Fatalf("ExpireJob: %v", err)
	}

	if pub.Kind != KindPublish || exp.Kind != KindExpire {
		t.Fatalf("kinds = %q and %q", pub.Kind, exp.Kind)
	}
	if !(pub.Priority < exp.Priority) {
		t.Errorf("publish is priority %d and expire is %d; the publish must run first",
			pub.Priority, exp.Priority)
	}
	if !pub.ScheduledFor.Equal(at) {
		t.Errorf("scheduled for %v, want %v", pub.ScheduledFor, at)
	}
	if err := pub.Normalize(at).Validate(); err != nil {
		t.Errorf("the publish job this builds is not one the store will take: %v", err)
	}
}

func TestOutputPath(t *testing.T) {
	for _, tc := range []struct {
		fileURI string
		want    string
		wantErr bool
	}{
		{fileURI: "/features/film/a-piece/index.html", want: "features/film/a-piece/index.html"},
		{fileURI: "/index.html", want: "index.html"},

		// The three that would leave the tree, and the one that would write
		// the root itself. None of these can be typed: they come from a
		// category directory, a slug, and a channel's filename, and no column
		// constrains their characters.
		{fileURI: "/../etc/passwd", wantErr: true},
		{fileURI: "/a/../../b/index.html", wantErr: true},
		{fileURI: "/a//b/index.html", wantErr: true},
		{fileURI: "/", wantErr: true},
		{fileURI: "", wantErr: true},
		{fileURI: "/a/\x00/index.html", wantErr: true},
	} {
		t.Run(tc.fileURI, func(t *testing.T) {
			got, err := OutputPath(tc.fileURI)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("OutputPath(%q) = %q, want a refusal", tc.fileURI, got)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("OutputPath(%q) = %v, want invalid", tc.fileURI, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("OutputPath(%q): %v", tc.fileURI, err)
			}
			if got != tc.want {
				t.Errorf("OutputPath(%q) = %q, want %q", tc.fileURI, got, tc.want)
			}
		})
	}
}

// TestParseScheduleIsParseDuesGrammar is the consistency somebody notices only
// when it is missing.
func TestParseScheduleIsParseDuesGrammar(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{"2h", "2026-03-02", "2026-03-02T09:00:00Z"} {
		schedule, err := ParseSchedule(in, now)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", in, err)
		}
		due, err := ParseDue(in, now)
		if err != nil {
			t.Fatalf("ParseDue(%q): %v", in, err)
		}
		if !schedule.Equal(due) {
			t.Errorf("%q reads as %v as a schedule and %v as a due date", in, schedule, due)
		}
	}
	if _, err := ParseSchedule("next tuesday", now); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unparseable instant was accepted: %v", err)
	}
	if _, err := ParseSchedule("-2h", now); !errors.Is(err, ErrInvalid) {
		t.Errorf("a negative duration was accepted: %v", err)
	}
}
