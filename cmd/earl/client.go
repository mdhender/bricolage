// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The HTTP client earl speaks with.
//
// It lives in cmd/earl rather than under internal/ because it has exactly one
// caller and because DESIGN.md 4 names no client package. If a second thing
// ever needs to speak this API from Go, this is what moves.

// DefaultTimeout bounds a request. earl is interactive and a hung command with
// no timeout is a command somebody kills with the wrong signal.
const DefaultTimeout = 30 * time.Second

// Client talks to one server.
type Client struct {
	// Server is the origin: scheme and host, no trailing slash. It is the
	// public origin, not the Go listener -- talking to 127.0.0.1:18443
	// directly bypasses the proxy and exercises a path that does not exist in
	// production (DESIGN.md 11).
	Server string

	// Token authenticates as a bearer, when there is one.
	Token string

	HTTP *http.Client
}

// NewClient builds a client for an origin.
func NewClient(server, token string) (*Client, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("server %q: %w", server, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("server %q: needs a scheme and a host, like https://host:port", server)
	}
	return &Client{
		Server: strings.TrimRight(u.Scheme+"://"+u.Host, "/"),
		Token:  token,
		HTTP: &http.Client{
			Timeout: DefaultTimeout,
			// Redirects are not followed automatically. The development login
			// route answers 302 when returnTo is given, and following it would
			// discard the response earl came for.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Problem is the RFC 9457 document the API returns for a refusal
// (DESIGN.md 12).
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Guard    string `json:"guard"`

	// Template and Line name the template that failed and where, when one
	// did (PLAN.md M8 acceptance 5). They survive production's rule that a
	// 500 carries no detail, which is the point of them being extension
	// members rather than prose.
	Template string `json:"template"`
	Line     int    `json:"line"`
}

// Error renders a problem the way a person wants to read it: the title, the
// detail, and the request id if there is one to quote at somebody.
func (p *Problem) Error() string {
	var b strings.Builder
	b.WriteString(p.Title)
	if p.Detail != "" {
		b.WriteString(": ")
		b.WriteString(p.Detail)
	}
	if p.Guard != "" {
		fmt.Fprintf(&b, " (guard %s)", p.Guard)
	}
	if p.Template != "" {
		fmt.Fprintf(&b, " (template %s", p.Template)
		if p.Line > 0 {
			fmt.Fprintf(&b, " line %d", p.Line)
		}
		b.WriteString(")")
	}
	if p.Instance != "" {
		fmt.Fprintf(&b, " [request %s]", p.Instance)
	}
	return b.String()
}

// Do performs a request against path and decodes the response into out.
//
// A 4xx or 5xx is decoded as a problem document and returned as an error, so
// every caller gets one error type and none of them has to check a status
// code.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		// A bearer token, never the cookie. It is what exempts the request
		// from the CSRF protection, and it is what a non-browser client should
		// be sending anyway (DESIGN.md 11).
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, c.Server+path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s %s: reading the response: %w", method, c.Server+path, err)
	}

	if resp.StatusCode >= 400 {
		return problemFrom(resp, payload)
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s %s: decoding the response: %w", method, c.Server+path, err)
	}
	return nil
}

// problemFrom turns an error response into a *Problem, falling back to the
// status line when the body is not a problem document -- which is what a proxy
// returns when it, and not the server, is refusing.
func problemFrom(resp *http.Response, payload []byte) error {
	var p Problem
	if err := json.Unmarshal(payload, &p); err == nil && p.Title != "" {
		if p.Status == 0 {
			p.Status = resp.StatusCode
		}
		return &p
	}
	detail := strings.TrimSpace(string(payload))
	if len(detail) > 500 {
		detail = detail[:500] + "..."
	}
	return &Problem{
		Status: resp.StatusCode,
		Title:  resp.Status,
		Detail: detail,
	}
}

// Get is Do for a request with no body.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// GetText fetches a path and returns the body as text.
//
// Two routes answer with something other than a JSON document: the development
// login route, which answers with a token, and /preview/{name}, which answers
// with a rendered page. Both are read through here.
func (c *Client) GetText(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Server+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "*/*")
	if c.Token != "" {
		// The preview mount requires a live session, like every other route
		// that reads content. The development login route has no token yet
		// and sends none.
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", c.Server+path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxTextResponse))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", problemFrom(resp, payload)
	}
	return string(payload), nil
}

// maxTextResponse bounds a body read as text. A rendered page is the largest
// thing this client fetches, and eight megabytes is more than any page a
// person will read and small enough that a misconfigured server cannot fill
// this process's memory.
const maxTextResponse = 8 << 20
