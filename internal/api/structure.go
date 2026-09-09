// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"fmt"
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// The category, output channel, element type, and URI routes (DESIGN.md 12,
// PLAN.md M7).
//
// Handlers parse a request, call one service method, and render the result.
// The one thing worth noting about the shapes below is that a category is
// named by its path as often as by its uid: "/features/film/" is what a person
// types and what a grant carries, and requiring a client to look up an
// identifier to file a story in a section it already knows the name of would
// be a round trip that buys nothing. The uid is still what a mutation
// addresses (invariant 10); the path is a lookup key on one site, and the
// UNIQUE (site_id, path) index is what makes it one.

// categoryResponse is a category as the API speaks it.
type categoryResponse struct {
	UID  string `json:"uid"`
	Site int64  `json:"site"`
	Path string `json:"path"`
	Name string `json:"name"`

	// Directory is the single segment this category adds to its parent's
	// path. It is empty for a site's root.
	Directory string `json:"directory,omitempty"`

	// Parent is the parent's path, absent for a site's root.
	Parent string `json:"parent,omitempty"`

	// Depth is how many segments the path carries; the root is 0. It is here
	// so that a client can indent a listing without parsing the path.
	Depth int `json:"depth"`
}

func newCategoryResponse(c domain.Category, parentPath string) categoryResponse {
	return categoryResponse{
		UID:       c.UID,
		Site:      c.SiteID,
		Path:      c.Path,
		Name:      c.Name,
		Directory: c.Directory,
		Parent:    parentPath,
		Depth:     c.Depth(),
	}
}

// parentPathOf derives a category's parent path from its own.
//
// It is arithmetic rather than a second query: the path is materialised and
// '/'-terminated, so the parent is everything up to the last interior slash.
// A root has no parent and gets the empty string.
func parentPathOf(c domain.Category) string {
	if c.IsRoot() {
		return ""
	}
	return c.Path[:len(c.Path)-len(c.Directory)-1]
}

// listCategories is GET /api/v1/categories?site=N.
func (h *Handler) listCategories(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	site, err := int64Param(r, "site")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if site == 0 {
		h.writeError(w, r, fmt.Errorf("site: required; a category tree belongs to one site: %w", domain.ErrInvalid))
		return
	}
	cats, err := h.svc.Categories(r.Context(), identity, site)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]categoryResponse, 0, len(cats))
	for _, c := range cats {
		out = append(out, newCategoryResponse(c, parentPathOf(c)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": out})
}

// createCategoryRequest is the body of POST /api/v1/categories.
type createCategoryRequest struct {
	Site      int64  `json:"site"`
	Parent    string `json:"parent,omitempty"`
	Directory string `json:"directory"`
	Name      string `json:"name"`
}

func (h *Handler) createCategory(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req createCategoryRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	// An omitted parent is the site's root, which is what a person means by
	// "make a section". It is a default rather than a requirement because
	// every site has a root and naming it every time buys nothing.
	parent := req.Parent
	if parent == "" {
		parent = domain.RootPath
	}
	c, err := h.svc.CreateCategory(r.Context(), identity, domain.NewCategory{
		SiteID:     req.Site,
		ParentPath: parent,
		Directory:  req.Directory,
		Name:       req.Name,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newCategoryResponse(c, parentPathOf(c)))
}

func (h *Handler) showCategory(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	c, err := h.svc.Category(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newCategoryResponse(c, parentPathOf(c)))
}

// moveCategoryRequest is the body of PATCH /api/v1/categories/{uid}.
//
// Both fields are pointers because a rename leaves the parent alone and a move
// leaves the directory alone, and the two together are one operation: a
// section that is renamed and moved in two requests is a section whose URIs
// change twice.
type moveCategoryRequest struct {
	Parent    *string `json:"parent,omitempty"`
	Directory *string `json:"directory,omitempty"`
}

// patchCategory moves or renames a category, which rewrites every descendant's
// path in one statement (PLAN.md M7 acceptance 1).
func (h *Handler) patchCategory(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req moveCategoryRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	c, err := h.svc.MoveCategory(r.Context(), identity, r.PathValue("uid"), domain.CategoryMove{
		ParentPath: req.Parent,
		Directory:  req.Directory,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newCategoryResponse(c, parentPathOf(c)))
}

func (h *Handler) deleteCategory(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := h.svc.DeleteCategory(r.Context(), identity, r.PathValue("uid")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// filingResponse is one place a document sits.
type filingResponse struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Primary  bool   `json:"primary"`
}

func newFilingResponses(filings []domain.Filing) []filingResponse {
	out := make([]filingResponse, 0, len(filings))
	for _, f := range filings {
		out = append(out, filingResponse{
			Category: f.Category.Path,
			Name:     f.Category.Name,
			Primary:  f.Primary,
		})
	}
	return out
}

func (h *Handler) listDocumentCategories(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	_, filings, err := h.svc.DocumentCategories(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": newFilingResponses(filings)})
}

// fileDocumentRequest is the body of PUT /api/v1/documents/{uid}/categories.
//
// The first path is the primary category, which is what the URI is built from.
// Making it the first element rather than a separate field means "these are
// the categories, this one first" is one list rather than a list and a pointer
// into it that can disagree with itself.
type fileDocumentRequest struct {
	Categories []string `json:"categories"`
}

// fileDocument is PUT /api/v1/documents/{uid}/categories.
//
// It is a subresource and not a field on PATCH /documents/{uid}, which is a
// departure from DESIGN.md 12's original list and the same departure M5 made
// for the due date. The reason is the lease: PATCH writes the working draft
// and needs a checkout, while document_categories rows point at the document
// rather than at a version. There is no draft copy of a filing to protect, and
// requiring a checkout to refile a story would make the most ordinary bulk
// operation a newsroom performs -- moving a section -- impossible while
// anybody was writing in it.
//
// PUT rather than POST because it replaces: "these are the categories" is the
// request a client makes, and computing the difference here means the client
// cannot send two requests that half succeed.
func (h *Handler) fileDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req fileDocumentRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	doc, filings, err := h.svc.FileDocument(r.Context(), identity, r.PathValue("uid"), req.Categories)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":        doc.UID,
		"categories": newFilingResponses(filings),
	})
}

// uriResponse is one address a document has.
type uriResponse struct {
	Channel string `json:"channel"`
	Name    string `json:"name"`

	URI  string `json:"uri,omitempty"`
	File string `json:"file,omitempty"`
	URL  string `json:"url,omitempty"`

	// Error is why this channel produces no address, when it produces none.
	// It is per channel rather than a failed request, because "this story has
	// no cover date and the news channel needs one" is a fact about one
	// channel and the others still have answers.
	Error string `json:"error,omitempty"`
}

// documentURIs is GET /api/v1/documents/{uid}/uris.
//
// It is not in DESIGN.md 12's list, and it is here because a URI format is
// configuration a person types and gets wrong, and the only alternative to
// showing them what it produces is publishing something to find out.
func (h *Handler) documentURIs(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, uris, err := h.svc.DocumentURIs(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]uriResponse, 0, len(uris))
	for _, u := range uris {
		row := uriResponse{
			Channel: u.Channel.UID,
			Name:    u.Channel.Name,
			URI:     u.URI,
			File:    u.File,
			URL:     u.URL,
		}
		if u.Err != nil {
			row.Error = u.Err.Error()
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":  view.Document.UID,
		"uris": out,
	})
}

// channelResponse is an output channel as the API speaks it.
type channelResponse struct {
	UID            string `json:"uid"`
	Site           int64  `json:"site"`
	Name           string `json:"name"`
	Protocol       string `json:"protocol"`
	Filename       string `json:"filename"`
	FileExt        string `json:"file_ext"`
	URIFormat      string `json:"uri_format"`
	FixedURIFormat string `json:"fixed_uri_format"`
	UseSlug        bool   `json:"use_slug"`
	URICase        string `json:"uri_case"`
}

func newChannelResponse(oc domain.OutputChannel) channelResponse {
	return channelResponse{
		UID:            oc.UID,
		Site:           oc.SiteID,
		Name:           oc.Name,
		Protocol:       oc.Protocol,
		Filename:       oc.Filename,
		FileExt:        oc.FileExt,
		URIFormat:      oc.URIFormat,
		FixedURIFormat: oc.FixedURIFormat,
		UseSlug:        oc.UseSlug,
		URICase:        oc.URICase,
	}
}

func (h *Handler) listOutputChannels(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	site, err := int64Param(r, "site")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	channels, err := h.svc.OutputChannels(r.Context(), identity, site)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]channelResponse, 0, len(channels))
	for _, oc := range channels {
		out = append(out, newChannelResponse(oc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"output_channels": out})
}

func (h *Handler) showOutputChannel(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	oc, err := h.svc.OutputChannel(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newChannelResponse(oc))
}

// channelRequest is the body of POST and PATCH on /api/v1/output-channels.
//
// Every field but the site is optional and defaulted by the service, because a
// caller that omits a URI format is asking for the ordinary one rather than
// for an empty one.
type channelRequest struct {
	Site           int64  `json:"site,omitempty"`
	Name           string `json:"name,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
	Filename       string `json:"filename,omitempty"`
	FileExt        string `json:"file_ext,omitempty"`
	URIFormat      string `json:"uri_format,omitempty"`
	FixedURIFormat string `json:"fixed_uri_format,omitempty"`
	UseSlug        bool   `json:"use_slug,omitempty"`
	URICase        string `json:"uri_case,omitempty"`
}

func (req channelRequest) channel() domain.OutputChannel {
	return domain.OutputChannel{
		SiteID:         req.Site,
		Name:           req.Name,
		Protocol:       req.Protocol,
		Filename:       req.Filename,
		FileExt:        req.FileExt,
		URIFormat:      req.URIFormat,
		FixedURIFormat: req.FixedURIFormat,
		UseSlug:        req.UseSlug,
		URICase:        req.URICase,
	}
}

func (h *Handler) createOutputChannel(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req channelRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	oc, err := h.svc.CreateOutputChannel(r.Context(), identity, req.channel())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newChannelResponse(oc))
}

func (h *Handler) patchOutputChannel(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req channelRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	oc, err := h.svc.UpdateOutputChannel(r.Context(), identity, r.PathValue("uid"), req.channel())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newChannelResponse(oc))
}

// elementTypeRequest is the body of POST /api/v1/element-types and of
// PATCH /api/v1/element-types/{key}.
//
// The mutable fields are pointers because an omitted field and a false one are
// different requests on a PATCH: leaving fixed_uri alone and turning it off
// are two things, and the second changes every address a document of that type
// has.
type elementTypeRequest struct {
	KeyName   string  `json:"key_name,omitempty"`
	Name      *string `json:"name,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	TopLevel  *bool   `json:"top_level,omitempty"`
	FixedURI  *bool   `json:"fixed_uri,omitempty"`
	Paginated *bool   `json:"paginated,omitempty"`
	Schema    *string `json:"schema,omitempty"`
}

func (h *Handler) createElementType(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req elementTypeRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	in := service.NewElementTypeRequest{KeyName: req.KeyName, Kind: req.Kind}
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.TopLevel != nil {
		in.TopLevel = *req.TopLevel
	}
	if req.FixedURI != nil {
		in.FixedURI = *req.FixedURI
	}
	if req.Paginated != nil {
		in.Paginated = *req.Paginated
	}
	if req.Schema != nil {
		in.Schema = *req.Schema
	}

	et, err := h.svc.CreateElementType(r.Context(), identity, in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newElementTypeResponse(et))
}

func (h *Handler) patchElementType(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req elementTypeRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if req.KeyName != "" || req.Kind != "" {
		// Both decide who may touch every document of the type: the key name
		// is what a document names its type by, and the kind is a grant scope
		// dimension. Refusing is better than accepting and ignoring.
		h.writeError(w, r, fmt.Errorf(
			"an element type's key name and kind are fixed at creation: %w", domain.ErrInvalid))
		return
	}
	et, err := h.svc.UpdateElementType(r.Context(), identity, r.PathValue("key"), service.ElementTypeUpdate{
		Name:      req.Name,
		TopLevel:  req.TopLevel,
		FixedURI:  req.FixedURI,
		Paginated: req.Paginated,
		Schema:    req.Schema,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newElementTypeResponse(et))
}

func (h *Handler) showElementType(w http.ResponseWriter, r *http.Request, _ domain.Identity) {
	et, err := h.svc.ElementType(r.Context(), r.PathValue("key"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newElementTypeResponse(et))
}

// siteResponse is a site as the API speaks it.
//
// The id is an integer rather than the uid invariant 10 asks for, and it is
// deliberate rather than an oversight: sites.uid exists but nothing has ever
// spoken it, "site": 1 is what POST /documents has taken since M3, and
// changing it here would leave the two disagreeing. M8 has a reason to pay
// that off; M7 does not, and half a rename is worse than none.
type siteResponse struct {
	ID     int64  `json:"id"`
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Active bool   `json:"active"`
}

func (h *Handler) listSites(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	sites, err := h.svc.Sites(r.Context(), identity)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]siteResponse, 0, len(sites))
	for _, s := range sites {
		out = append(out, siteResponse{
			ID: s.ID, UID: s.UID, Name: s.Name, Domain: s.Domain, Active: s.Active,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}
