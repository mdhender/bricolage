// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// Output channels, element types, and the URIs they produce (PLAN.md M7).
//
// Two different privileges, for two different reasons.
//
// An output channel is a publishing decision -- it says where content is
// written and what its address is -- so it needs Publish over the site it
// belongs to. A person who may not publish anything on a site has no business
// deciding what that site's URLs look like, and the person who may is exactly
// the person who would have to fix it.
//
// An element type is not on a site at all: it has no site_id, it applies
// across every site, and its schema decides what every document of that type
// may contain. It is therefore resolved against the *system subject* -- the
// empty domain.Subject, which only a grant constraining nothing matches --
// which is the same rule DESIGN.md 12 states for the job queue and for the
// same reason: a site-scoped grant says what its holder may do to that site's
// documents and says nothing about a definition every site shares.

// The privileges the structure operations need.
const (
	// ChannelAdmin is what writing an output channel needs, over its site.
	ChannelAdmin = domain.Publish

	// ElementTypeAdmin is what writing an element type needs, over the system
	// subject.
	ElementTypeAdmin = domain.Create

	// SiteAdmin is what changing a site needs, over the system subject
	// (issue #3).
	//
	// Over the system and not over the site, which is the one place this
	// departs from ChannelAdmin beside it. A grant scoped to a site says what
	// its holder may do to that site's content, and an output channel's URI
	// format is exactly that: a decision about how this site's documents are
	// addressed. A site's domain is not. It is the first path segment of the
	// template tree, so changing it moves where every one of this site's
	// templates is looked for -- a change to the shape of the installation,
	// made by whoever lays that tree out, rather than to the site's content.
	// It is also the privilege creating an additional site will need, and
	// there is no site to scope that one to.
	SiteAdmin = domain.Create
)

// systemSubject is the empty subject: only a grant that constrains nothing
// matches it. It is what an object belonging to no site and no category is
// resolved against.
func systemSubject() domain.Subject { return domain.Subject{} }

// OutputChannels returns one site's output channels, or every site's when
// siteID is 0, filtered to what the caller may read.
func (s *Service) OutputChannels(ctx context.Context, actor domain.Identity, siteID int64) ([]domain.OutputChannel, error) {
	all, err := s.db.ListOutputChannels(ctx, siteID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.OutputChannel, 0, len(all))
	for _, oc := range all {
		if authz.Allows(actor.Grants, domain.Subject{SiteID: oc.SiteID}, domain.Read) {
			out = append(out, oc)
		}
	}
	return out, nil
}

// OutputChannel reads one output channel.
func (s *Service) OutputChannel(ctx context.Context, actor domain.Identity, uid string) (domain.OutputChannel, error) {
	oc, err := s.db.OutputChannelByUID(ctx, uid)
	if err != nil {
		return domain.OutputChannel{}, err
	}
	if !authz.Allows(actor.Grants, domain.Subject{SiteID: oc.SiteID}, domain.Read) {
		return domain.OutputChannel{}, fmt.Errorf("output channel %q: %w", uid, domain.ErrNotFound)
	}
	return oc, nil
}

// CreateOutputChannel writes an output channel.
//
// The defaults are filled in here rather than in the schema's DEFAULT clauses,
// because a caller that omits a URI format is asking for the ordinary one
// rather than for an empty one, and a channel with an empty format would build
// no address at all. domain.OutputChannel.Validate then refuses what is left,
// including a format naming a strftime conversion this system does not
// implement -- which is a thing to discover when the format is written rather
// than when a publish runs.
func (s *Service) CreateOutputChannel(ctx context.Context, actor domain.Identity, oc domain.OutputChannel) (domain.OutputChannel, error) {
	if oc.SiteID == 0 {
		return domain.OutputChannel{}, fmt.Errorf("output channel: no site: %w", domain.ErrInvalid)
	}
	if !authz.Allows(actor.Grants, domain.Subject{SiteID: oc.SiteID}, ChannelAdmin) {
		return domain.OutputChannel{}, fmt.Errorf(
			"output channels on site %d: %s is required: %w", oc.SiteID, ChannelAdmin, domain.ErrForbidden)
	}
	if _, err := s.db.SiteByID(ctx, oc.SiteID); err != nil {
		return domain.OutputChannel{}, err
	}

	oc = withChannelDefaults(oc)
	now := s.Now()
	uid, err := ids.New(now)
	if err != nil {
		return domain.OutputChannel{}, err
	}
	oc.UID = uid
	return s.db.CreateOutputChannel(ctx, oc, domain.Event{
		Type:       events.OutputChannelCreated,
		ActorID:    actor.User.ID,
		Payload:    channelPayload(oc),
		OccurredAt: now,
	})
}

// UpdateOutputChannel replaces an output channel's configuration.
func (s *Service) UpdateOutputChannel(ctx context.Context, actor domain.Identity, uid string, in domain.OutputChannel) (domain.OutputChannel, error) {
	existing, err := s.db.OutputChannelByUID(ctx, uid)
	if err != nil {
		return domain.OutputChannel{}, err
	}
	if !authz.Allows(actor.Grants, domain.Subject{SiteID: existing.SiteID}, ChannelAdmin) {
		return domain.OutputChannel{}, fmt.Errorf(
			"output channel %q: %s is required: %w", uid, ChannelAdmin, domain.ErrForbidden)
	}

	// The site and the uid are the channel's own; everything else is the
	// request's. An update that could move a channel between sites would
	// change the domain every address it has produced resolves against.
	in.ID = existing.ID
	in.UID = existing.UID
	in.SiteID = existing.SiteID
	in = withChannelDefaults(in)

	now := s.Now()
	return s.db.UpdateOutputChannel(ctx, in, domain.Event{
		Type:       events.OutputChannelUpdated,
		ActorID:    actor.User.ID,
		Payload:    channelPayload(in),
		OccurredAt: now,
	})
}

// withChannelDefaults fills in what a caller left out.
func withChannelDefaults(oc domain.OutputChannel) domain.OutputChannel {
	if oc.Protocol == "" {
		oc.Protocol = domain.DefaultProtocol
	}
	if oc.Filename == "" {
		oc.Filename = domain.DefaultFilename
	}
	if oc.FileExt == "" {
		oc.FileExt = domain.DefaultFileExt
	}
	if oc.URIFormat == "" {
		oc.URIFormat = domain.DefaultURIFormat
	}
	if oc.FixedURIFormat == "" {
		oc.FixedURIFormat = domain.DefaultFixedURIFormat
	}
	if oc.URICase == "" {
		oc.URICase = domain.URICaseMixed
	}
	return oc
}

// channelPayload is what an output channel event records.
//
// The formats are carried verbatim, unlike a document body: a format edited
// last month is what last month's addresses were built from, and a
// configuration row shows only what it says today.
func channelPayload(oc domain.OutputChannel) map[string]any {
	return map[string]any{
		"uid":              oc.UID,
		"site":             oc.SiteID,
		"name":             oc.Name,
		"uri_format":       oc.URIFormat,
		"fixed_uri_format": oc.FixedURIFormat,
		"use_slug":         oc.UseSlug,
		"uri_case":         oc.URICase,
		"filename":         oc.Filename,
		"file_ext":         oc.FileExt,
	}
}

// NewElementTypeRequest is an element type somebody is asking to create.
type NewElementTypeRequest struct {
	KeyName   string
	Name      string
	Kind      string
	TopLevel  bool
	FixedURI  bool
	Paginated bool
	Schema    string
}

// CreateElementType writes an element type.
//
// The schema is parsed before it is stored, so that a schema declaring a field
// type nobody implements is refused when it is written. Left unchecked it
// would refuse every document of that type at check-in with a message about
// the document, which sends the wrong person looking.
func (s *Service) CreateElementType(ctx context.Context, actor domain.Identity, in NewElementTypeRequest) (domain.ElementType, error) {
	if !authz.Allows(actor.Grants, systemSubject(), ElementTypeAdmin) {
		return domain.ElementType{}, fmt.Errorf(
			"element types: %s over everything is required: %w", ElementTypeAdmin, domain.ErrForbidden)
	}
	if in.KeyName == "" {
		return domain.ElementType{}, fmt.Errorf("element type: a key name is required: %w", domain.ErrInvalid)
	}
	if !domain.ValidDocKind(in.Kind) {
		return domain.ElementType{}, fmt.Errorf(
			"element type kind %q: not one of story, media, template: %w", in.Kind, domain.ErrInvalid)
	}
	schema, err := domain.ParseElementSchema(in.Schema)
	if err != nil {
		return domain.ElementType{}, err
	}

	now := s.Now()
	uid, err := ids.New(now)
	if err != nil {
		return domain.ElementType{}, err
	}
	name := in.Name
	if name == "" {
		name = in.KeyName
	}
	return s.db.CreateElementType(ctx, store.NewElementType{
		UID:       uid,
		KeyName:   in.KeyName,
		Name:      name,
		Kind:      in.Kind,
		TopLevel:  in.TopLevel,
		FixedURI:  in.FixedURI,
		Paginated: in.Paginated,
		Schema:    domain.NormalizeContent(in.Schema),
		CreatedAt: now,
		Event: domain.Event{
			Type:    events.ElementTypeCreated,
			ActorID: actor.User.ID,
			Payload: map[string]any{
				"uid": uid, "key_name": in.KeyName, "kind": in.Kind,
				"fixed_uri": in.FixedURI, "fields": fieldNames(schema),
			},
			OccurredAt: now,
		},
	})
}

// ElementTypeUpdate is a change to an element type. Every field is a pointer
// because an omitted field and an empty one are different requests.
type ElementTypeUpdate struct {
	Name      *string
	TopLevel  *bool
	FixedURI  *bool
	Paginated *bool
	Schema    *string
}

// IsEmpty reports whether the update asks for nothing.
func (u ElementTypeUpdate) IsEmpty() bool {
	return u.Name == nil && u.TopLevel == nil && u.FixedURI == nil &&
		u.Paginated == nil && u.Schema == nil
}

// UpdateElementType changes an element type's configuration.
//
// The key name and the kind are not changeable: the key name is what a client
// names the type by (invariant 10) and the kind is a grant scope dimension, so
// changing it would silently change who may touch every document of that type.
func (s *Service) UpdateElementType(ctx context.Context, actor domain.Identity, keyName string, u ElementTypeUpdate) (domain.ElementType, error) {
	if !authz.Allows(actor.Grants, systemSubject(), ElementTypeAdmin) {
		return domain.ElementType{}, fmt.Errorf(
			"element types: %s over everything is required: %w", ElementTypeAdmin, domain.ErrForbidden)
	}
	if u.IsEmpty() {
		return domain.ElementType{}, fmt.Errorf("element type: nothing to change: %w", domain.ErrInvalid)
	}
	et, err := s.db.ElementTypeByKeyName(ctx, keyName)
	if err != nil {
		return domain.ElementType{}, err
	}

	if u.Name != nil {
		et.Name = *u.Name
	}
	if u.TopLevel != nil {
		et.TopLevel = *u.TopLevel
	}
	if u.FixedURI != nil {
		et.FixedURI = *u.FixedURI
	}
	if u.Paginated != nil {
		et.Paginated = *u.Paginated
	}
	if u.Schema != nil {
		et.Schema = domain.NormalizeContent(*u.Schema)
	}
	schema, err := domain.ParseElementSchema(et.Schema)
	if err != nil {
		return domain.ElementType{}, err
	}

	now := s.Now()
	return s.db.UpdateElementType(ctx, et, domain.Event{
		Type:    events.ElementTypeUpdated,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid": et.UID, "key_name": et.KeyName, "kind": et.Kind,
			"fixed_uri": et.FixedURI, "fields": fieldNames(schema),
		},
		OccurredAt: now,
	})
}

// fieldNames is what an element type event records instead of the schema.
func fieldNames(schema domain.ElementSchema) []string {
	out := make([]string, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		out = append(out, f.Name)
	}
	return out
}

// DocumentURI is one address a document has: which channel, and where.
type DocumentURI struct {
	Channel domain.OutputChannel

	// URI is the path the document is served at, and File is the file written
	// for it. Both are empty when Err is set.
	URI  string
	File string
	URL  string

	// Err is why this channel produces no address, when it produces none. It
	// is carried per channel rather than failing the request, because "this
	// story has no cover date and the news channel needs one" is a fact about
	// one channel and the others still have answers.
	Err error
}

// DocumentURIs computes a document's address in every output channel of its
// site (PLAN.md M7).
//
// It is the visible face of domain.BuildURI, and it exists so that a person
// can see what a URI format does before a publish depends on it -- which is
// the whole reason a URI format is worth having a command for. The computation
// itself is pure; everything this method does is fetch the four things it
// takes.
func (s *Service) DocumentURIs(ctx context.Context, actor domain.Identity, uid string) (DocumentView, []DocumentURI, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, nil, err
	}
	view, err := s.viewOf(ctx, doc)
	if err != nil {
		return DocumentView{}, nil, err
	}

	// A document filed nowhere has no address, and saying so is better than
	// building one from the root that nobody would recognise as an accident.
	filings, err := s.db.CategoriesForDocument(ctx, doc.ID)
	if err != nil {
		return DocumentView{}, nil, err
	}
	primary, filed := domain.PrimaryOf(filings)
	if !filed {
		return view, nil, fmt.Errorf(
			"document %s is not filed in any category, so it has no address: %w", uid, domain.ErrConflict)
	}

	site, err := s.db.SiteByID(ctx, doc.SiteID)
	if err != nil {
		return DocumentView{}, nil, err
	}
	channels, err := s.db.ListOutputChannels(ctx, doc.SiteID)
	if err != nil {
		return DocumentView{}, nil, err
	}
	if len(channels) == 0 {
		return view, nil, fmt.Errorf(
			"site %d has no output channel, so nothing says what a URI looks like: %w",
			doc.SiteID, domain.ErrConflict)
	}

	out := make([]DocumentURI, 0, len(channels))
	for _, oc := range channels {
		row := DocumentURI{Channel: oc}
		uri, err := domain.BuildURI(doc, view.Version, primary, oc)
		if err != nil {
			row.Err = err
		} else {
			row.URI = uri
			row.File = oc.FileURI(uri)
			row.URL = oc.URL(site.Domain, uri)
		}
		out = append(out, row)
	}
	return view, out, nil
}

// SiteUpdate is a change to a site. Both fields are pointers because an
// omitted field and an empty one are different requests.
//
// There is deliberately no Active. Nothing in this system reads sites.active
// yet -- not the publisher, not the resolver, not a route -- so a verb that
// wrote it would be one that appeared to switch a publication off and did not.
// It arrives with the code that honours it.
type SiteUpdate struct {
	Name   *string
	Domain *string
}

// IsEmpty reports whether the update asks for nothing.
func (u SiteUpdate) IsEmpty() bool { return u.Name == nil && u.Domain == nil }

// UpdateSite changes a site's name or its domain (issue #3).
//
// This is what a deployment needs and did not have. "cmsdb seed" writes a
// domain, and a server brought up on a real host had no supported way to
// correct it: the alternatives were to name the production template directory
// after a development hostname or to UPDATE the row by hand, outside
// internal/store, on the outermost scope dimension of every grant.
//
// The uid is fixed and so is the site's identity. What moves is the name it is
// spelled by, which is a rename and not a new site: categories and grants key
// on site_id, and no URI format carries the domain, so the schema is untouched
// by it. What does move is every template lookup -- the tree hangs off
// <templates>/<site domain>/ -- and every absolute URL the site's channels
// build, which is why the event carries the old value as well as the new one.
// A caller renaming a live site has a directory to rename at the same instant,
// and nothing here can do that for it: the template tree is not this system's
// to write (invariant 19).
func (s *Service) UpdateSite(ctx context.Context, actor domain.Identity, uid string, u SiteUpdate) (domain.Site, error) {
	if !authz.Allows(actor.Grants, systemSubject(), SiteAdmin) {
		return domain.Site{}, fmt.Errorf(
			"sites: %s over everything is required: %w", SiteAdmin, domain.ErrForbidden)
	}
	if u.IsEmpty() {
		return domain.Site{}, fmt.Errorf("site: nothing to change: %w", domain.ErrInvalid)
	}
	site, err := s.db.SiteByUID(ctx, uid)
	if err != nil {
		return domain.Site{}, err
	}
	was := site

	if u.Name != nil {
		if err := domain.ValidateSiteName(*u.Name); err != nil {
			return domain.Site{}, err
		}
		site.Name = strings.TrimSpace(*u.Name)
	}
	if u.Domain != nil {
		normalized, err := domain.NormalizeSiteDomain(*u.Domain)
		if err != nil {
			return domain.Site{}, err
		}
		site.Domain = normalized
	}

	return s.db.UpdateSite(ctx, site, domain.Event{
		Type:    events.SiteUpdated,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid": site.UID, "name": site.Name, "domain": site.Domain,
			"was_name": was.Name, "was_domain": was.Domain,
		},
		OccurredAt: s.Now(),
	})
}

// ElementType reads one element type by key name.
func (s *Service) ElementType(ctx context.Context, keyName string) (domain.ElementType, error) {
	et, err := s.db.ElementTypeByKeyName(ctx, keyName)
	if err != nil && errors.Is(err, domain.ErrNotFound) {
		return domain.ElementType{}, fmt.Errorf("element type %q: %w", keyName, domain.ErrNotFound)
	}
	return et, err
}

// Sites returns the sites the caller may read.
//
// It is here because a client has to name a site to create a document or a
// category, and until now the only way to learn a site id was to be told one
// out of band. The id is still an integer in the response, because sites.uid
// exists but nothing has ever spoken it and the document routes take
// "site": 1; that is a wart M8 can pay off when it has a reason to.
func (s *Service) Sites(ctx context.Context, actor domain.Identity) ([]domain.Site, error) {
	all, err := s.db.ListSites(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Site, 0, len(all))
	for _, site := range all {
		if authz.Allows(actor.Grants, domain.Subject{SiteID: site.ID}, domain.Read) {
			out = append(out, site)
		}
	}
	return out, nil
}
