// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// Publishing's vocabulary (DESIGN.md 8.1, 8.3, PLAN.md M9).
//
// Everything here is pure. What internal/publish adds to it is the two things
// a pure package cannot do: render, and write a file.
//
// The idea the whole milestone turns on is in PublishPayload: a publish job
// names a document_version_id and never a document_id (invariant 8). An editor
// approves version 5 for midnight and keeps working; at midnight version 5
// publishes, not whatever the draft has become. The immutability trigger on
// document_versions is what makes that promise real, and a design that models
// this as "publish the current draft at time T" cannot express it at all.

// The job kinds publishing schedules.
//
// They are here rather than in internal/jobs beside KindNoop because three
// packages have to agree on them and none of the three may import the others:
// internal/workflow builds a publish job as a transition effect,
// internal/publish handles both kinds, and internal/service schedules one from
// an API request. A constant in the package every one of them already imports
// is how they agree without a dependency edge that would not survive.
const (
	// KindPublish renders a pinned version to an output channel and records
	// what it wrote.
	KindPublish = "publish"

	// KindExpire deletes a file the publisher no longer produces, and the
	// resource row it was recorded by is already gone: expiry is scheduled by
	// the publish that stopped producing the address, in the transaction that
	// stopped recording it.
	KindExpire = "expire"
)

// There is deliberately no SubjectResource.
//
// A resource's history is kept against the document rather than against the
// row: the row is deleted when the address stops being produced, and an audit
// trail whose subject can vanish is an audit trail with holes in it. A subject
// kind nothing writes would be a name that lies about what the events table
// holds (invariant 6).

// Resource is one file the publisher has written (DESIGN.md 8.3).
//
// URI is the address within the site and Path is where the bytes are on disk,
// beneath the output tree's root. They are two fields rather than one because
// the address is what a link points at and what the expiry diff compares,
// while the path is what has to be deleted; for an ordinary channel the second
// is the first plus "index.html", and for a channel with a different filename
// it is not.
type Resource struct {
	ID              int64
	DocumentID      int64
	OutputChannelID int64
	VersionID       int64

	URI      string
	Path     string
	Checksum string
	Bytes    int64

	PublishedAt time.Time

	// DocumentUID and ChannelName are joined in by the store so that the API
	// can name both without a second query. Neither is a column here.
	DocumentUID string
	ChannelName string
}

// PublishPayload is what a publish job carries (DESIGN.md 8.1).
//
// VersionID is the pin and the reason this type exists. DocumentID is carried
// beside it because every failure message and every event wants to name the
// document, and reading the version to find out which document it belongs to
// before being able to say what failed is a query in the error path.
//
// ChannelIDs empty means every output channel of the document's site. That is
// what "publish this" means when nobody says otherwise, and it is deliberately
// resolved when the job runs rather than when it is scheduled: a channel added
// between the schedule and the publish is a channel the site wants, and a job
// scheduled for next Tuesday that silently ignores it would be a bug nobody
// could see.
type PublishPayload struct {
	DocumentID int64 `json:"document_id"`
	VersionID  int64 `json:"document_version_id"`

	ChannelIDs []int64 `json:"output_channel_ids,omitempty"`

	// ActorID is who asked, or 0 for the system. It is the actor of the
	// events the publish writes, so that "who put this live" is a question
	// the history answers.
	ActorID int64 `json:"actor_id,omitempty"`
}

// Validate reports whether the payload is one a handler can act on.
func (p PublishPayload) Validate() error {
	if p.DocumentID <= 0 {
		return fmt.Errorf("publish job: no document: %w", ErrInvalid)
	}
	if p.VersionID <= 0 {
		return fmt.Errorf("publish job for document %d: no version to pin; a publish job names a version and never a document (invariant 8): %w",
			p.DocumentID, ErrInvalid)
	}
	return nil
}

// JSON renders the payload for jobs.payload.
func (p PublishPayload) JSON() (string, error) { return marshalPayload("publish", p) }

// ParsePublishPayload reads a publish job's payload.
func ParsePublishPayload(s string) (PublishPayload, error) {
	var p PublishPayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return PublishPayload{}, fmt.Errorf("publish job: payload: %v: %w", err, ErrInvalid)
	}
	return p, p.Validate()
}

// ExpirePayload is what an expire job carries.
//
// It names a path and not a resource id, because by the time the job runs the
// resource row is gone: the publish that stopped producing the address deleted
// the row and scheduled this in the same transaction. A job that had to read a
// row to learn what to delete would be a job that could never delete anything.
type ExpirePayload struct {
	DocumentID      int64  `json:"document_id"`
	OutputChannelID int64  `json:"output_channel_id"`
	URI             string `json:"uri"`
	Path            string `json:"path"`

	ActorID int64 `json:"actor_id,omitempty"`
}

// Validate reports whether the payload is one a handler can act on.
func (p ExpirePayload) Validate() error {
	if strings.TrimSpace(p.Path) == "" {
		return fmt.Errorf("expire job: no path to delete: %w", ErrInvalid)
	}
	return nil
}

// JSON renders the payload for jobs.payload.
func (p ExpirePayload) JSON() (string, error) { return marshalPayload("expire", p) }

// ParseExpirePayload reads an expire job's payload.
func ParseExpirePayload(s string) (ExpirePayload, error) {
	var p ExpirePayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return ExpirePayload{}, fmt.Errorf("expire job: payload: %v: %w", err, ErrInvalid)
	}
	return p, p.Validate()
}

func marshalPayload(kind string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("%s job: encoding the payload: %w", kind, err)
	}
	return string(b), nil
}

// PublishJob is the job a publish schedules.
//
// It is a constructor rather than a struct literal at each call site because
// there are three of them -- the API, the transition effect, and a retry --
// and a publish job enqueued at the wrong priority or with the pin left out is
// a bug that only shows up at the scheduled hour.
//
// PriorityHigh rather than PriorityNormal: somebody is waiting for a page to
// appear, and the level below it is where a bulk republish lives.
func PublishJob(p PublishPayload, at time.Time) (NewJob, error) {
	if err := p.Validate(); err != nil {
		return NewJob{}, err
	}
	payload, err := p.JSON()
	if err != nil {
		return NewJob{}, err
	}
	return NewJob{
		Kind:         KindPublish,
		Priority:     PriorityHigh,
		ScheduledFor: at,
		Payload:      payload,
		CreatedBy:    p.ActorID,
	}, nil
}

// ExpireJob is the job a stale address schedules.
//
// PriorityLow: the file is already unreferenced by the database and nobody is
// waiting for it to go. What matters is that it goes, not when, and a queue
// that ran expiry ahead of a publish would make a slug change take a page down
// before putting the new one up.
func ExpireJob(p ExpirePayload, at time.Time) (NewJob, error) {
	if err := p.Validate(); err != nil {
		return NewJob{}, err
	}
	payload, err := p.JSON()
	if err != nil {
		return NewJob{}, err
	}
	return NewJob{
		Kind:         KindExpire,
		Priority:     PriorityLow,
		ScheduledFor: at,
		Payload:      payload,
		CreatedBy:    p.ActorID,
	}, nil
}

// ParseSchedule reads the instant a publish is asked for.
//
// It is ParseDue's grammar under a different noun: a date, a timestamp, or a
// duration from now. "publish in 2h" is what somebody means far more often
// than a timestamp they had to compute, and the two readers being one function
// is why "48h" cannot mean two things in one system.
func ParseSchedule(s string, now time.Time) (time.Time, error) {
	return ParseInstant("publish time", s, now)
}

// OutputPath is where the bytes for a URI live beneath the output tree.
//
// It is the file URI with its leading slash removed, because the output root
// is the site's "/" and a path beginning with one would be an absolute path on
// the machine rather than an address within the tree. Everything else about it
// is OutputChannel.FileURI's arithmetic, done once here so that the writer,
// the reader, and "cmsdb check" cannot invent three of them.
//
// It refuses a path that would leave the tree. The segments come from a
// category path, a slug, and an output channel's filename, none of which is
// constrained to safe characters by any column, and "../../etc/passwd" as a
// slug would otherwise be a write outside the root with a migration in front
// of it. The write itself goes through an os.Root as well, so this is the
// first of two answers rather than the only one.
func OutputPath(fileURI string) (string, error) {
	trimmed := strings.TrimPrefix(fileURI, "/")
	if trimmed == "" {
		return "", fmt.Errorf("output path %q: empty: %w", fileURI, ErrInvalid)
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", fmt.Errorf("output path %q: contains a NUL: %w", fileURI, ErrInvalid)
	}
	if path.Clean(trimmed) != trimmed {
		return "", fmt.Errorf("output path %q: not a clean relative path: %w", fileURI, ErrInvalid)
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("output path %q: %q is not a path segment: %w", fileURI, segment, ErrInvalid)
		}
	}
	return trimmed, nil
}

// There is deliberately no Go statement of the stale-expiry diff here.
//
// One DELETE performs it, in internal/store, and it returns the rows it
// removed in the same statement (DESIGN.md 8.3). A pure function beside it
// that nothing called would be a second statement of the rule, and two
// statements of one rule is how they come to disagree -- which is the failure
// this whole milestone is about, at a smaller scale.
