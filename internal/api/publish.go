// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// Publishing (DESIGN.md 12, PLAN.md M9).
//
// Two routes. POST .../publications schedules a publish, GET .../resources
// says what is at the document's addresses now.
//
// "Publications" is a collection because a publish is a thing that happened at
// an instant, not a property of the document: two publishes of one document
// are two events with two versions behind them, and a PUT that set
// "published: true" could not say which version or when. The plural is the
// design's own (DESIGN.md 12) and it is the right shape.

// publicationRequest is the body of POST /api/v1/documents/{uid}/publications.
type publicationRequest struct {
	// At is when the publish should happen, RFC 3339. Absent or empty means
	// now.
	At string `json:"at,omitempty"`

	// Channels are the output channels' uids. Empty means every channel of
	// the document's site, resolved when the job runs.
	Channels []string `json:"channels,omitempty"`
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
}

type publicationChannel struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
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
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	out := publicationResponse{
		UID:          result.View.Document.UID,
		Version:      result.Version.Number,
		Job:          result.Job.UID,
		ScheduledFor: result.Job.ScheduledFor,
	}
	for _, oc := range result.Channels {
		out.Channels = append(out.Channels, publicationChannel{UID: oc.UID, Name: oc.Name})
	}

	// 202 rather than 201. Nothing has been published yet: a job has been
	// scheduled, and a publish for next Tuesday answered with "created" would
	// be telling the client the page exists.
	writeJSON(w, http.StatusAccepted, out)
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
