// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/edge"
	"github.com/mdhender/bricolage/internal/reqctx"
)

// ContentTypeProblem is the media type of an RFC 9457 problem document.
const ContentTypeProblem = "application/problem+json"

// problemBase namespaces the "type" of a problem document. RFC 9457 wants a
// URI that identifies the problem type; it does not have to resolve, but it
// does have to be stable, because clients switch on it.
const problemBase = "https://mdhender.github.io/bricolage/errors/"

// Problem is an RFC 9457 problem document (DESIGN.md 12).
//
// It carries extension members -- "guard" is the one DESIGN.md 12 shows -- so
// that a client can act on the specific refusal without parsing prose out of
// "detail".
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`

	// Instance is the request id, so that a generic production error and a
	// log line can be joined up (DESIGN.md 14).
	Instance string `json:"instance,omitempty"`

	// Guard names the workflow guard that refused, when one did.
	Guard string `json:"guard,omitempty"`

	// Errors carries field-level detail for a 422.
	Errors []FieldError `json:"errors,omitempty"`

	// Template and Line name the template that failed and where, when one
	// did (PLAN.md M8 acceptance 5).
	Template string `json:"template,omitempty"`
	Line     int    `json:"line,omitempty"`

	// Refusals name the documents a related-asset cascade would not publish,
	// when one refused (PLAN.md M10 acceptance 2). They are an extension
	// member rather than prose in "detail" for the reason "guard" is: a
	// client showing an editor which stories are holding the publish up has
	// to be able to list them without parsing a sentence.
	Refusals []publicationRefusal `json:"refusals,omitempty"`
}

// FieldError is one invalid field in a 422.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// statusFor maps a domain error to an HTTP status.
//
// The table itself is internal/edge's, because M13 gave this repository a
// second transport and DESIGN.md 14 asks for one function rather than one per
// edge. This wrapper stays because every caller here reads better for it and
// because the package's own tests name it.
func statusFor(err error) (status int, kind, title string) {
	return edge.StatusFor(err)
}

// writeError renders err as a problem document.
//
// What reaches the client depends on the environment (DESIGN.md 14): in
// development the underlying detail, because the person reading it is the
// person who caused it; in production a generic message and a request id,
// because the person reading it is not, and an error message is a disclosure
// channel. A 4xx is the caller's own mistake and says so in both, since
// telling somebody their body did not parse discloses nothing they did not
// send.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, kind, title := statusFor(err)

	p := Problem{
		Type:     problemBase + kind,
		Title:    title,
		Status:   status,
		Instance: reqctx.RequestID(r.Context()),
	}

	// A guard refusal names the guard, which is the extension member
	// DESIGN.md 12 shows. It is filled in here rather than at the handler,
	// because this is the one function that turns an error into a response and
	// a second place that knew about guards would be a second place to forget.
	if g, ok := domain.GuardOf(err); ok {
		p.Guard = string(g)
	}

	// Content that does not match its element type names every offending
	// field (PLAN.md M7 acceptance 5). It is filled in here for the same
	// reason the guard is: this is the one function that turns an error into a
	// response, and a second place that knew about field errors would be a
	// second place to forget.
	if fields, ok := domain.FieldErrorsOf(err); ok {
		p.Errors = make([]FieldError, 0, len(fields))
		for _, f := range fields {
			p.Errors = append(p.Errors, FieldError{Field: f.Field, Message: f.Message})
		}
	}

	// A broken template names itself and the line it broke at
	// (PLAN.md M8 acceptance 5). It is an extension member rather than part
	// of the detail because a template failure is a 500, and a 500's detail
	// is generic in production -- the person who has to fix the template
	// would otherwise be told only that something went wrong. The name of a
	// template on this server is not a secret; the stack behind it is, and
	// that stays in the log.
	if te, ok := domain.TemplateErrorOf(err); ok {
		p.Template = te.Template
		p.Line = te.Line
	}

	// A refused cascade names every document it would have had to publish and
	// could not. Filled in here for the same reason as the three above: this
	// is the one function that turns an error into a response, and a second
	// place that knew about refusals would be a second place to forget.
	if refusals, ok := domain.RefusalsOf(err); ok {
		p.Refusals = newPublicationRefusals(refusals)
	}

	switch {
	case status < http.StatusInternalServerError:
		p.Detail = err.Error()
	case errors.Is(err, domain.ErrUnavailable):
		// A 503 says this server was not configured to answer the request,
		// and its message names the flag that was not given. It is written
		// for the operator rather than derived from an internal failure, and
		// withholding it would leave an authenticated caller with "something
		// went wrong" about a thing nobody can fix without being told which
		// thing it is.
		p.Detail = err.Error()
	case h.env.IsDevelopment():
		p.Detail = err.Error()
	default:
		p.Detail = "the server could not handle this request"
	}

	if status >= http.StatusInternalServerError {
		h.log.Error("request failed",
			"status", status,
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", p.Instance,
			"error", err,
		)
	} else {
		h.log.Log(r.Context(), slog.LevelDebug, "request refused",
			"status", status,
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", p.Instance,
			"error", err,
		)
	}

	writeProblem(w, p)
}

// writeProblem serialises a problem document.
func writeProblem(w http.ResponseWriter, p Problem) {
	body, err := json.Marshal(p)
	if err != nil {
		// A problem document that will not marshal is a programming error,
		// and the client still needs an answer.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// writeJSON renders a successful response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeProblem(w, Problem{
			Type:   problemBase + "internal",
			Title:  "Internal error",
			Status: http.StatusInternalServerError,
			Detail: "the response could not be encoded",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// decodeJSON reads a request body into v, refusing anything that is not one
// JSON object.
//
// Unknown fields are refused. A client sending {"e-mail": ...} has made a
// mistake, and accepting it silently means the mistake is discovered later, by
// somebody else, as an empty column.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return fmt.Errorf("body: empty: %w", domain.ErrInvalid)
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("body: %v: %w", err, domain.ErrInvalid)
	}
	if dec.More() {
		return fmt.Errorf("body: more than one JSON value: %w", domain.ErrInvalid)
	}
	return nil
}

// decodeJSONOptional is decodeJSON for a route whose body is optional.
//
// An empty body is not a mistake on POST .../revoke: the only thing in it is a
// reason, and revoking without giving one is an ordinary thing to do. A body
// that is present is held to the same rules, so a misspelled "resaon" is still
// refused rather than silently dropped.
func decodeJSONOptional(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	// The body is read rather than probed, because ContentLength is -1 on a
	// chunked request and a "was it empty" question answered from the header
	// would be answered wrongly there. Classifying an empty body by catching
	// the decoder's io.EOF would mean reading its message, which is the habit
	// invariant 11 exists to break -- so emptiness is decided here, on the
	// bytes, and everything else goes through decodeJSON unchanged.
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("body: %v: %w", err, domain.ErrInvalid)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return decodeJSON(r, v)
}

// maxBodyBytes bounds a request body. Nothing this milestone accepts is large,
// and an unbounded decoder is an easy denial of service.
const maxBodyBytes = 1 << 20
