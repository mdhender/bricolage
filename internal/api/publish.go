// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// Publishing (DESIGN.md 12, PLAN.md M9 and M10).
//
// Two routes. POST .../publications schedules a publish, GET .../resources
// says what is at the document's addresses now.
//
// "Publications" is a collection because a publish is a thing that happened at
// an instant, not a property of the document: two publishes of one document
// are two events with two versions behind them, and a PUT that set
// "published: true" could not say which version or when. The plural is the
// design's own (DESIGN.md 12) and it is the right shape.
//
// M10 gives the route two more things to say. "related" is the documents the
// cascade gathered beside the root, each with the version it pinned, and
// "refusals" names the ones that will not be published and why -- by uid,
// because a report that said "3 documents could not be published" is a report
// nobody can act on. "dry_run" asks for both without scheduling anything, and
// answers 200 rather than 202: nothing was accepted for processing.

// publicationRequest is the body of POST /api/v1/documents/{uid}/publications.
type publicationRequest struct {
	// At is when the publish should happen, RFC 3339. Absent or empty means
	// now.
	At string `json:"at,omitempty"`

	// Channels are the output channels' uids. Empty means every channel of
	// the document's site, resolved when the job runs.
	Channels []string `json:"channels,omitempty"`

	// DryRun gathers the related-asset cascade and reports it without
	// scheduling anything (DESIGN.md 12, PLAN.md M10 acceptance 6).
	DryRun bool `json:"dry_run,omitempty"`
}

// publicationResponse is a scheduled publish as the API speaks it.
type publicationResponse struct {
	UID string `json:"uid"`

	// Version is the version the job pinned, and it is the point of the
	// response. A publish names a version and never a document
	// (invariant 8); telling the client which one is how the promise becomes
	// visible rather than implied.
	Version int `json:"version"`

	// Job is the scheduled work's uid (invariant 10), and ScheduledFor is
	// when it may start.
	Job          string    `json:"job"`
	ScheduledFor time.Time `json:"scheduled_for"`

	// Channels are the channels named, absent when every channel of the site
	// is meant.
	Channels []publicationChannel `json:"channels,omitempty"`

	// Related are the documents the cascade gathered beside the root, in
	// traversal order, each with the version it pinned and the job scheduled
	// for it (PLAN.md M10). Absent when the document references nothing.
	Related []publicationRelated `json:"related,omitempty"`

	// Refusals are the referenced documents that will not be published, each
	// named. Under the "fail" policy a real publish never carries these -- it
	// is a 409 whose problem document does -- so a non-empty list here is
	// either a dry run or the "warn" policy reporting what it went ahead
	// without.
	Refusals []publicationRefusal `json:"refusals,omitempty"`

	// DryRun echoes the request, and WouldRefuse says the policy would have
	// aborted. A client that could not tell a plan from an outcome would
	// draw "published" over a page that is not.
	DryRun      bool `json:"dry_run,omitempty"`
	WouldRefuse bool `json:"would_refuse,omitempty"`
}

type publicationChannel struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// publicationRelated is one document the cascade gathered.
type publicationRelated struct {
	UID     string `json:"uid"`
	Title   string `json:"title,omitempty"`
	Version int    `json:"version"`

	// Job is empty on a dry run, which schedules none.
	Job string `json:"job,omitempty"`
}

// publicationRefusal is one document the cascade will not publish.
type publicationRefusal struct {
	UID          string `json:"uid"`
	Title        string `json:"title,omitempty"`
	ReferencedBy string `json:"referenced_by,omitempty"`
	Reason       string `json:"reason"`
	Detail       string `json:"detail"`
}

// newPublicationRefusals renders refusals for a response body. It is shared
// with the problem document, which carries the same list on a 409, so that a
// client parses one shape whichever answer it gets.
func newPublicationRefusals(refusals []domain.Refusal) []publicationRefusal {
	out := make([]publicationRefusal, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, publicationRefusal{
			UID:          r.UID,
			Title:        r.Title,
			ReferencedBy: r.Referrer,
			Reason:       string(r.Reason),
			Detail:       r.Detail,
		})
	}
	return out
}

// resourceResponse is one published file as the API speaks it.
type resourceResponse struct {
	Channel     string    `json:"channel_name"`
	URI         string    `json:"uri"`
	Path        string    `json:"path"`
	Version     int64     `json:"version_id"`
	Checksum    string    `json:"checksum"`
	Bytes       int64     `json:"bytes"`
	PublishedAt time.Time `json:"published_at"`
}

// publishDocument is POST /api/v1/documents/{uid}/publications.
func (h *Handler) publishDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req publicationRequest
	// A body is optional: "publish this now, everywhere" is the ordinary
	// request, and requiring "{}" for it would be a rule with no reason.
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			h.writeError(w, r, err)
			return
		}
	}

	var at time.Time
	if req.At != "" {
		parsed, err := domain.ParseSchedule(req.At, h.svc.Now())
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		at = parsed
	}

	result, err := h.svc.Publish(r.Context(), identity, service.PublishRequest{
		UID:         r.PathValue("uid"),
		At:          at,
		ChannelUIDs: req.Channels,
		DryRun:      req.DryRun,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	root := result.Root()
	out := publicationResponse{
		UID:          result.View.Document.UID,
		Version:      root.Version.Number,
		Job:          root.Job.UID,
		ScheduledFor: root.Job.ScheduledFor,
		DryRun:       result.DryRun,
		WouldRefuse:  result.WouldRefuse,
	}
	if result.DryRun {
		// A dry run schedules nothing, so there is no job uid to name and no
		// instant a job may start at. What it can say is when the publish
		// would be asked for, which is the request's own instant with "now"
		// already resolved.
		out.ScheduledFor = result.At
	}
	for _, oc := range result.Channels {
		out.Channels = append(out.Channels, publicationChannel{UID: oc.UID, Name: oc.Name})
	}
	for _, sp := range result.Related() {
		out.Related = append(out.Related, publicationRelated{
			UID:     sp.Document.UID,
			Title:   sp.Version.Title,
			Version: sp.Version.Number,
			Job:     sp.Job.UID,
		})
	}
	if len(result.Refusals) > 0 {
		out.Refusals = newPublicationRefusals(result.Refusals)
	}

	// 202 rather than 201. Nothing has been published yet: a job has been
	// scheduled, and a publish for next Tuesday answered with "created" would
	// be telling the client the page exists.
	//
	// A dry run is 200 and not 202, because nothing at all was accepted for
	// processing: the answer is a report, and it is complete when it is read.
	status := http.StatusAccepted
	if result.DryRun {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// documentResources is GET /api/v1/documents/{uid}/resources.
func (h *Handler) documentResources(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, resources, err := h.svc.Resources(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	out := struct {
		UID       string             `json:"uid"`
		Live      int64              `json:"live_version_id,omitempty"`
		Resources []resourceResponse `json:"resources"`
	}{
		UID:       view.Document.UID,
		Live:      view.Document.LiveVersionID,
		Resources: make([]resourceResponse, 0, len(resources)),
	}
	for _, res := range resources {
		out.Resources = append(out.Resources, resourceResponse{
			Channel:     res.ChannelName,
			URI:         res.URI,
			Path:        res.Path,
			Version:     res.VersionID,
			Checksum:    res.Checksum,
			Bytes:       res.Bytes,
			PublishedAt: res.PublishedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
