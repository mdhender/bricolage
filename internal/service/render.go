// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/render"
)

// Preview and template validation (PLAN.md M8).
//
// Preview is a read. It needs Read over the document and nothing else, which
// is the decision worth stating: previewing does not change the document, does
// not take the edit lease, and does not need the privilege publishing needs. A
// writer who may edit a story may look at what it will become; requiring
// Publish would mean the only people who could check a template were the
// people who could put it live, which is the wrong way round.
//
// No event is written. Every state-changing operation writes one (invariant 7)
// and this changes no state: the scratch file is a pure function of a version,
// a template, and an output channel, and it can be produced again by asking
// again. An event per preview would be an audit log of people looking at
// things.

// PreviewRequest names what to render.
type PreviewRequest struct {
	// UID is the document.
	UID string

	// ChannelUID is the output channel, or empty when the site has exactly
	// one and the caller means that one.
	ChannelUID string

	// Validate asks for domain.ModeValidate: parse the template, report what
	// is wrong with it, and write nothing.
	Validate bool
}

// PreviewResult is a rendered preview, or the report of a template that would
// not parse.
type PreviewResult struct {
	View     DocumentView
	Site     domain.Site
	Category domain.Category
	Channel  domain.OutputChannel

	// Mode is the mode that ran: domain.ModePreview or domain.ModeValidate.
	Mode string

	// URI is the address the document would have in this channel, and URL is
	// the absolute form. They are what the template was given, so a preview
	// shows the same links the published page will.
	URI string
	URL string

	// Template is the template that was chosen and Searched is every path
	// that was tried, deepest first. Searched is carried on success because
	// "which template did this use, and what did it beat" is the question
	// asked when the wrong one is used.
	Template string
	Searched []string

	// Entry is the written preview. It is the zero value in validate mode,
	// which writes nothing.
	Entry render.Entry

	// Valid and Problem are the answer in validate mode. A template that will
	// not parse is reported rather than returned as an error, for the reason
	// DocumentURIs reports a channel that can build no address: the question
	// asked was "does this compile", and "no, at line 12" is an answer.
	Valid   bool
	Problem *domain.TemplateError
}

// Preview renders one document into the scratch tree, or validates its
// template.
func (s *Service) Preview(ctx context.Context, actor domain.Identity, req PreviewRequest) (PreviewResult, error) {
	if s.renderer == nil {
		return PreviewResult{}, fmt.Errorf(
			"this server was started without a template tree, so it renders nothing; give cmsd --templates DIR: %w",
			domain.ErrUnavailable)
	}

	doc, err := s.mayRead(ctx, actor, req.UID)
	if err != nil {
		return PreviewResult{}, err
	}
	view, err := s.viewOf(ctx, doc)
	if err != nil {
		return PreviewResult{}, err
	}
	if view.Version.ID == 0 {
		return PreviewResult{}, fmt.Errorf(
			"document %s has no version to render: %w", req.UID, domain.ErrConflict)
	}

	// The primary category is what the URI is built from and what the
	// template cascade is searched from, so a document filed nowhere has
	// neither. Saying so is better than rendering it at the root, which would
	// be an address nobody would recognise as an accident.
	filings, err := s.db.CategoriesForDocument(ctx, doc.ID)
	if err != nil {
		return PreviewResult{}, err
	}
	primary, filed := domain.PrimaryOf(filings)
	if !filed {
		return PreviewResult{}, fmt.Errorf(
			"document %s is not filed in any category, so there is no address to render it at: %w",
			req.UID, domain.ErrConflict)
	}

	site, err := s.db.SiteByID(ctx, doc.SiteID)
	if err != nil {
		return PreviewResult{}, err
	}
	channel, err := s.previewChannel(ctx, doc, req.ChannelUID)
	if err != nil {
		return PreviewResult{}, err
	}

	uri, err := domain.BuildURI(doc, view.Version, primary, channel)
	if err != nil {
		return PreviewResult{}, err
	}

	mode := domain.ModePreview
	if req.Validate {
		mode = domain.ModeValidate
	}
	out := PreviewResult{
		View:     view,
		Site:     site,
		Category: primary,
		Channel:  channel,
		Mode:     mode,
		URI:      uri,
		URL:      channel.URL(site.Domain, uri),
	}

	result, err := s.renderer.Render(render.Input{
		Mode:     mode,
		Site:     site,
		Channel:  channel,
		Category: primary,
		Document: doc,
		Version:  view.Version,
		URI:      out.URI,
		URL:      out.URL,
	})
	out.Template = result.Template
	out.Searched = result.Searched

	switch {
	case err == nil:
		out.Valid = true
	case req.Validate:
		// Validate mode reports a broken template rather than failing
		// (PLAN.md M8 acceptance 2). A missing template is not a broken one
		// and is still an error: "there is no template" and "the template does
		// not compile" send different people looking.
		te, ok := domain.TemplateErrorOf(err)
		if !ok {
			return PreviewResult{}, err
		}
		out.Problem = te
		return out, nil
	default:
		return PreviewResult{}, err
	}

	if req.Validate {
		return out, nil
	}

	if s.preview == nil {
		return PreviewResult{}, fmt.Errorf(
			"this server was started without a preview tree, so a preview has nowhere to go; give cmsd --preview DIR: %w",
			domain.ErrUnavailable)
	}
	entry, err := s.preview.Put(result.Body, channel.FileExt)
	if err != nil {
		return PreviewResult{}, err
	}
	out.Entry = entry
	return out, nil
}

// PreviewOpen reads a written preview back, for the /preview/ handler.
//
// There is no document to authorize against: a preview is named by the SHA-256
// of its own bytes, so the name is not a reference to a document that could be
// resolved to a grant. What guards it is that the route requires a live
// session and that a name nobody has been given cannot be guessed -- knowing
// it means having been told it, and being told it meant passing the Read check
// in Preview.
func (s *Service) PreviewOpen(name string) (fs.File, error) {
	if s.preview == nil {
		return nil, fmt.Errorf(
			"this server was started without a preview tree; give cmsd --preview DIR: %w",
			domain.ErrUnavailable)
	}
	return s.preview.Open(name)
}

// PreviewConfigured reports whether this server can serve previews at all. It
// is what the startup banner and the log line ask.
func (s *Service) PreviewConfigured() bool { return s.renderer != nil && s.preview != nil }

// previewChannel resolves which output channel to render for.
//
// An unnamed channel is the site's, when the site has exactly one. It is a
// convenience with a limit: as soon as there are two, "the channel" is a
// question the caller has to answer, and picking the first would make a
// preview quietly show the wrong output of two correct ones.
func (s *Service) previewChannel(ctx context.Context, doc domain.Document, uid string) (domain.OutputChannel, error) {
	if strings.TrimSpace(uid) != "" {
		oc, err := s.db.OutputChannelByUID(ctx, uid)
		if err != nil {
			return domain.OutputChannel{}, err
		}
		if oc.SiteID != doc.SiteID {
			return domain.OutputChannel{}, fmt.Errorf(
				"output channel %s is on site %d and document %s is on site %d: %w",
				uid, oc.SiteID, doc.UID, doc.SiteID, domain.ErrInvalid)
		}
		return oc, nil
	}

	channels, err := s.db.ListOutputChannels(ctx, doc.SiteID)
	if err != nil {
		return domain.OutputChannel{}, err
	}
	switch len(channels) {
	case 0:
		return domain.OutputChannel{}, fmt.Errorf(
			"site %d has no output channel, so nothing says what this document's address or file looks like: %w",
			doc.SiteID, domain.ErrConflict)
	case 1:
		return channels[0], nil
	default:
		names := make([]string, 0, len(channels))
		for _, oc := range channels {
			names = append(names, oc.Name)
		}
		return domain.OutputChannel{}, fmt.Errorf(
			"site %d has %d output channels (%s); name the one to render: %w",
			doc.SiteID, len(channels), strings.Join(names, ", "), domain.ErrInvalid)
	}
}
