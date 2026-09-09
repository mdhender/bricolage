// Copyright (c) 2026 Michael D Henderson.

package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	texttemplate "text/template"

	"github.com/mdhender/bricolage/internal/domain"
)

// Options configure an Engine. Everything is resolved before New is called;
// this package reads no flags, no environment, and no configuration file.
type Options struct {
	// Root is the template tree's directory. It must already exist: nothing
	// in this system creates a directory (invariant 19), and a renderer that
	// made an empty tree when it could not find one would report "no
	// template" for every document on the site rather than "there is no
	// template tree here".
	Root string

	// FS overrides Root, for a test with templates in memory. When it is set
	// Root is used only in messages.
	FS fs.FS

	// Reload re-reads and re-parses a template on every render, which is what
	// development does; production parses each once and keeps it
	// (DESIGN.md 14).
	Reload bool
}

// Engine parses and executes templates out of one tree.
//
// It is safe for concurrent use: the cache is guarded, and a parsed
// html/template is only ever executed, never modified, after it goes in.
type Engine struct {
	fsys   fs.FS
	root   string
	reload bool

	mu     sync.Mutex
	parsed map[string]*htmltemplate.Template
}

// New opens a template tree.
func New(opts Options) (*Engine, error) {
	e := &Engine{
		root:   opts.Root,
		reload: opts.Reload,
		parsed: map[string]*htmltemplate.Template{},
	}
	switch {
	case opts.FS != nil:
		e.fsys = opts.FS
	case strings.TrimSpace(opts.Root) == "":
		return nil, fmt.Errorf("render: no template root: %w", domain.ErrUnavailable)
	default:
		info, err := os.Stat(opts.Root)
		if err != nil {
			return nil, fmt.Errorf("template root %s: %w", opts.Root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("template root %s: not a directory", opts.Root)
		}
		e.fsys = os.DirFS(opts.Root)
	}
	return e, nil
}

// Root is the directory the tree was opened from, for a log line and for the
// message that says where a template was not found.
func (e *Engine) Root() string { return e.root }

// Input is everything one render needs. It is domain types rather than a
// prepared context because assembling the context is this package's job and
// the caller's job is to fetch the five rows.
type Input struct {
	// Mode is one of domain.ModePublish, ModePreview, ModeValidate.
	Mode string

	Site     domain.Site
	Channel  domain.OutputChannel
	Category domain.Category
	Document domain.Document
	Version  domain.Version

	// URI and URL are the address this render is for, as domain.BuildURI
	// computed it. They are passed in rather than computed here because
	// BuildURI is pure and has one caller's worth of inputs already gathered.
	URI string
	URL string
}

// Result is what a render produced.
type Result struct {
	// Template is the path of the template that was used.
	Template string

	// Searched is every path that was tried, deepest first.
	Searched []string

	// Body is the rendered bytes. It is nil in validate mode, which parses
	// and writes nothing.
	Body []byte
}

// Render looks up a template and runs it.
//
// The bytes are returned rather than written. That is what makes PLAN.md M8
// acceptance 5 -- an execution failure writes no partial file -- a property of
// the shape rather than of a deferred cleanup: there is no file to be partial
// until a caller writes one, and the caller only gets bytes on success.
func (e *Engine) Render(in Input) (Result, error) {
	if !domain.ValidRenderMode(in.Mode) {
		return Result{}, fmt.Errorf("render mode %q: not one of %s: %w",
			in.Mode, strings.Join(domain.RenderModes, ", "), domain.ErrInvalid)
	}
	if in.Document.ElementTypeKey == "" {
		return Result{}, fmt.Errorf("document %s: no element type key to find a template by: %w",
			in.Document.UID, domain.ErrInvalid)
	}

	loc := Locator{FS: e.fsys, Base: in.Site.Domain}
	match, err := loc.Lookup(in.Category.Path, in.Document.ElementTypeKey)
	if err != nil {
		return Result{}, err
	}

	tmpl, err := e.template(match.Path)
	if err != nil {
		return Result{Template: match.Path, Searched: match.Searched}, err
	}
	if in.Mode == domain.ModeValidate {
		return Result{Template: match.Path, Searched: match.Searched}, nil
	}

	ctx, err := newContext(in)
	if err != nil {
		return Result{Template: match.Path, Searched: match.Searched}, err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return Result{Template: match.Path, Searched: match.Searched},
			templateFailure(match.Path, domain.PhaseExecute, err)
	}
	return Result{Template: match.Path, Searched: match.Searched, Body: buf.Bytes()}, nil
}

// template returns the parsed template at p, parsing it if it has to.
//
// In development it parses every time, so that editing a template and
// reloading shows the edit. In production it parses once and keeps it, which
// is the row DESIGN.md 14 gives -- "parsed once at startup" -- done on first
// use rather than at startup, because nothing at startup knows which templates
// a running system will ask for and walking the whole tree to find out would
// make a broken template nobody uses a reason not to serve.
func (e *Engine) template(p string) (*htmltemplate.Template, error) {
	if !e.reload {
		e.mu.Lock()
		hit, ok := e.parsed[p]
		e.mu.Unlock()
		if ok {
			return hit, nil
		}
	}

	source, err := fs.ReadFile(e.fsys, p)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", p, err)
	}
	tmpl, err := htmltemplate.New(p).Funcs(funcs).Parse(string(source))
	if err != nil {
		return nil, templateFailure(p, domain.PhaseParse, err)
	}

	if !e.reload {
		e.mu.Lock()
		e.parsed[p] = tmpl
		e.mu.Unlock()
	}
	return tmpl, nil
}

// funcs is the function map every template gets. There is one function in it
// and adding a second needs a reason.
//
// "raw" is the escape hatch html/template is designed to have. A block field
// holds a paragraph "possibly with markup" (DESIGN.md 5.2), and a renderer
// that escapes it renders the markup as visible angle brackets, which makes
// the whole package useless for the thing it exists to do. What makes it
// acceptable is who writes the content: an editor holding Edit on the
// document, inside an authenticated system, which is the same trust every CMS
// extends to the people who use it. What makes it survivable is that a preview
// is served with a sandboxing Content-Security-Policy (see Scratch), so a
// script that arrives this way runs in an opaque origin and cannot reach the
// session cookie of the person previewing it.
var funcs = htmltemplate.FuncMap{
	"raw": func(s string) htmltemplate.HTML { return htmltemplate.HTML(s) },
}

// Context is what a template is executed against.
//
// It is a flat set of named pieces rather than the domain structs themselves,
// so that a template says {{.Version.Title}} and not {{.Version.Number}} next
// to fields a template has no business reading -- a lock owner, a workflow id,
// a row id. The rule for what is in it is "what a page could want to show".
type Context struct {
	// Mode is the render mode, and Preview is the one a template actually
	// branches on: a preview draws a banner so that nobody mistakes it for
	// the published page.
	Mode    string
	Preview bool

	Site     SiteContext
	Channel  ChannelContext
	Category CategoryContext
	Document DocumentContext
	Version  VersionContext

	// URI is the address within the site and URL is the absolute one.
	URI string
	URL string

	// Content is the version's element tree, decoded. Numbers are decoded as
	// json.Number so that a template prints what was stored rather than a
	// float's idea of it, and so that two renders of one version produce one
	// set of bytes (PLAN.md M8 acceptance 3).
	Content map[string]any
}

// SiteContext is the publication the page belongs to.
type SiteContext struct {
	Name   string
	Domain string
}

// ChannelContext is where the page is going.
type ChannelContext struct {
	Name     string
	Filename string
	FileExt  string
}

// CategoryContext is where the page is filed.
type CategoryContext struct {
	Path      string
	Name      string
	Directory string
	Depth     int
}

// DocumentContext is the document's stable identity.
type DocumentContext struct {
	UID         string
	Kind        string
	ElementType string
	State       string
}

// VersionContext is the snapshot being rendered.
type VersionContext struct {
	Number    int
	Title     string
	Slug      string
	CoverDate string
	Note      string

	// Draft reports whether this is the open working draft rather than a
	// checked-in version. A preview of a checked-out document renders the
	// draft (PLAN.md M8 acceptance 4), and a template that wants to say so
	// can.
	Draft bool
}

// newContext assembles the context from one input.
func newContext(in Input) (Context, error) {
	content, err := decodeContent(in.Version.Content)
	if err != nil {
		return Context{}, err
	}
	return Context{
		Mode:    in.Mode,
		Preview: in.Mode == domain.ModePreview,
		Site: SiteContext{
			Name:   in.Site.Name,
			Domain: in.Site.Domain,
		},
		Channel: ChannelContext{
			Name:     in.Channel.Name,
			Filename: in.Channel.Filename,
			FileExt:  in.Channel.FileExt,
		},
		Category: CategoryContext{
			Path:      in.Category.Path,
			Name:      in.Category.Name,
			Directory: in.Category.Directory,
			Depth:     in.Category.Depth(),
		},
		Document: DocumentContext{
			UID:         in.Document.UID,
			Kind:        in.Document.Kind,
			ElementType: in.Document.ElementTypeKey,
			State:       in.Document.State,
		},
		Version: VersionContext{
			Number:    in.Version.Number,
			Title:     in.Version.Title,
			Slug:      in.Version.Slug,
			CoverDate: in.Version.CoverDate,
			Note:      in.Version.Note,
			Draft:     in.Version.CheckedInAt.IsZero(),
		},
		URI:     in.URI,
		URL:     in.URL,
		Content: content,
	}, nil
}

// decodeContent reads a version's element tree.
//
// An empty draft decodes to an empty map rather than to nil, so that
// {{.Content.body}} on a document nobody has written yet renders nothing
// instead of failing. Content that will not parse is an error: check-in
// validates against the element type's schema, but a working draft may be
// invalid (DESIGN.md 5.2) and previewing one is exactly when somebody wants to
// be told which.
func decodeContent(s string) (map[string]any, error) {
	out := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("content: not a JSON object: %v: %w", err, domain.ErrInvalid)
	}
	return out, nil
}

// templateFailure turns whatever html/template said into a domain.TemplateError
// naming the template and the line.
func templateFailure(name, phase string, err error) error {
	te := &domain.TemplateError{Template: name, Phase: phase, Err: err}

	// html/template reports an escaping failure as its own error type, which
	// carries the line as a field. That is the only structured line number
	// either package offers.
	var esc *htmltemplate.Error
	if errors.As(err, &esc) {
		te.Line = esc.Line
		if esc.Name != "" {
			te.Template = esc.Name
		}
		return te
	}

	// An execution failure is a *text/template.ExecError, which names the
	// template but puts the line only in the message.
	var exec texttemplate.ExecError
	if errors.As(err, &exec) && exec.Name != "" {
		te.Template = exec.Name
	}
	te.Line = lineOf(err.Error())
	return te
}

// templateLocation matches the "template: NAME:LINE" prefix both packages
// write. Parsing a message is not something to be proud of; there is no other
// way to learn the line of a parse error, and PLAN.md M8 acceptance 5 asks for
// it by name.
var templateLocation = regexp.MustCompile(`^template: [^:]*:(\d+)`)

// lineOf returns the line a template error message names, or 0.
func lineOf(msg string) int {
	m := templateLocation.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}
