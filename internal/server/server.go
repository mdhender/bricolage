// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/service"
)

// The reasons a shutdown can begin. There are three ways in and one way
// through (invariant 17); the reason exists so the log says which door was
// used, not so that the paths can differ.
const (
	ReasonSignal  = "signal"
	ReasonTimeout = "timeout"
)

// Options configure a Server. Everything here is resolved before New is
// called; this package does not read flags, the environment, or a file.
type Options struct {
	// Environment is the resolved environment. It is the only gate on the
	// /__development/* routes (invariant 16).
	Environment config.Environment

	// Addr is the listen address. It defaults to config.DefaultAddr, which is
	// loopback.
	Addr string

	// Timeout, when positive, begins a graceful shutdown after the duration.
	// Zero means never.
	Timeout time.Duration

	// DrainTimeout bounds the shutdown itself. Zero means
	// config.DefaultDrainTimeout.
	DrainTimeout time.Duration

	// Logger receives the structured log. A nil Logger discards.
	Logger *slog.Logger

	// Banner receives the plain-text startup banner. A nil Banner discards it;
	// cmsd passes os.Stderr. See Server.WriteBanner for why this is not just a
	// log line.
	Banner io.Writer

	// Resolution, when set, lets the banner say not just what the environment
	// is but where it came from.
	Resolution *config.Resolution

	// Service is the use-case layer. A nil Service builds the route table
	// without the handlers that would need it, which is what "cmsd routes"
	// does: it prints the table a configuration produces without opening a
	// database.
	Service *service.Service

	// DeclareRoutesWithoutService makes the table include the routes a
	// Service would carry, with handlers that refuse.
	//
	// It exists for "cmsd routes", whose whole job is to print what "serve"
	// would mount (DESIGN.md 11). A table that omitted the API because no
	// database was open would answer the question wrongly, and the question is
	// "are the development routes registered".
	DeclareRoutesWithoutService bool

	// Origin is the public origin: what cookies are written for, what
	// returnTo is validated against, and what the CSRF protection trusts
	// (DESIGN.md 11). Zero means config.DefaultPublicOrigin.
	Origin config.PublicOrigin

	// TrustedProxies are the networks X-Forwarded-* headers are honoured
	// from (invariant 14). Nil means config.DefaultTrustedProxies.
	TrustedProxies []*net.IPNet

	// Background is the job worker pool, or nil for a server that runs none
	// -- which is "--workers 0" and "cmsd routes" alike.
	//
	// It is here rather than started beside the server in main because of
	// invariant 17. Workers have to stop when the server stops, and a second
	// place that decided when that was would be a second shutdown path. This
	// one starts them, drains HTTP first, then cancels them and waits, which
	// is the order DESIGN.md 11 gives: stop accepting, drain in-flight
	// requests, let workers finish the current job or release its lease,
	// close the pool.
	Background Background
}

// Background is something the server runs alongside serving and stops as part
// of its one shutdown path.
//
// Run must return when its context ends, and only once whatever it started has
// stopped; the server waits on it. It is an interface rather than the concrete
// pool so that internal/server does not import internal/jobs: the composition
// root wires the two together, and a transport package that knew about the job
// queue would be a dependency pointing the wrong way (DESIGN.md 3).
type Background interface {
	Run(ctx context.Context) error
}

// Backgrounds runs several of them as one.
//
// It exists because M12 gives this server a second thing to run beside the job
// workers -- the alert dispatcher -- and the alternative was a second field
// here, a second goroutine in Run, and a second place that decided when
// background work stops. Invariant 17 is that graceful shutdown is one code
// path; a second field would not have been a second path, but the third one
// would have been.
//
// Run returns when every member has returned, so the promise the server relies
// on -- "Run returns only once whatever it started has stopped" -- is the
// promise this keeps. A member that fails does not stop the others: they are
// independent, and taking the queue down because the dispatcher could not read
// a row is the failure mode internal/jobs already refuses.
type Backgrounds []Background

// Run starts every member and waits for all of them.
func (bs Backgrounds) Run(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, b := range bs {
		if b == nil {
			continue
		}
		wg.Go(func() {
			if err := b.Run(ctx); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Server is cmsd's HTTP server: one route table, one shutdown path.
type Server struct {
	env          config.Environment
	addr         string
	timeout      time.Duration
	drainTimeout time.Duration
	log          *slog.Logger
	banner       io.Writer
	resolution   *config.Resolution

	svc        *service.Service
	declareAPI bool
	origin     config.PublicOrigin
	trusted    []*net.IPNet
	background Background

	// handler is the mux with the middleware around it: the request id, the
	// resolved client address, and the CSRF protection. It is what Serve
	// serves and what a test drives.
	handler http.Handler

	mux    *http.ServeMux
	routes []Route

	// shutdown carries the reason for the first shutdown request. It is
	// buffered so that RequestShutdown never blocks: the development route
	// calls it from a handler, and the shutdown it starts waits for that
	// handler to return.
	shutdown chan string

	// afterFunc is a seam for tests, defaulting to time.AfterFunc. The
	// shutdown timer is the one piece of this package that depends on the
	// passage of real time, and a test should not have to spend two seconds
	// proving that it fires.
	afterFunc func(time.Duration, func()) *time.Timer
}

// New builds a Server and its route table.
//
// The table is built here rather than when serving starts, so that
// "cmsd routes" can ask for it without binding a port.
func New(opts Options) (*Server, error) {
	addr := opts.Addr
	if addr == "" {
		addr = config.DefaultAddr
	}
	if err := config.ValidateAddr(addr); err != nil {
		return nil, err
	}
	if opts.Timeout < 0 {
		return nil, fmt.Errorf("timeout %s: must not be negative", opts.Timeout)
	}

	origin := opts.Origin
	if origin.URL == nil {
		var err error
		if origin, err = config.ParsePublicOrigin(config.DefaultPublicOrigin); err != nil {
			return nil, err
		}
	}
	trusted := opts.TrustedProxies
	if trusted == nil {
		var err error
		if trusted, err = config.ParseTrustedProxies(config.DefaultTrustedProxies); err != nil {
			return nil, err
		}
	}

	s := &Server{
		env:          opts.Environment,
		addr:         addr,
		timeout:      opts.Timeout,
		drainTimeout: opts.DrainTimeout,
		log:          opts.Logger,
		banner:       opts.Banner,
		resolution:   opts.Resolution,
		svc:          opts.Service,
		declareAPI:   opts.DeclareRoutesWithoutService,
		background:   opts.Background,
		origin:       origin,
		trusted:      trusted,
		shutdown:     make(chan string, 1),
		afterFunc:    time.AfterFunc,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.drainTimeout <= 0 {
		s.drainTimeout = config.DefaultDrainTimeout
	}

	s.mux, s.routes = s.buildRoutes()

	// The middleware order is the order the values become available. The
	// request id first, so that every log line below it can carry one; the
	// client address next, so that a handler and the CSRF refusal see the same
	// answer; the CSRF protection last, closest to the mux, because it is the
	// only one that may refuse.
	protected, err := withCSRF(s.origin, s.mux)
	if err != nil {
		return nil, err
	}
	s.handler = withRequestID(withClientAddr(s.trusted, protected))

	return s, nil
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.addr }

// Resolution renders where the environment came from, in the same words the
// startup banner uses. "cmsd routes" prints it above the table, so the table
// is never read without the setting that produced it.
func (s *Server) Resolution() string {
	if s.resolution == nil {
		return "environment=" + s.env.String()
	}
	return s.resolution.String()
}

// Routes returns the table, exactly as registered.
func (s *Server) Routes() []Route { return s.routes }

// Handler returns what Serve serves: the mux with the middleware around it.
//
// A test drives this rather than the bare mux, so that what a test exercises
// is what a client reaches. Reaching past the middleware is how a CSRF
// exemption or a forged X-Forwarded-For gets tested into existence.
func (s *Server) Handler() http.Handler { return s.handler }

// Mux returns the bare route table, without the middleware. It is here for the
// tests that are about registration rather than about a request.
func (s *Server) Mux() http.Handler { return s.mux }

// Origin returns the configured public origin.
func (s *Server) Origin() config.PublicOrigin { return s.origin }

// RequestShutdown begins a graceful shutdown, naming the reason.
//
// It never blocks and it is safe to call more than once; only the first reason
// is kept, because a shutdown already under way is not made more graceful by
// being asked again.
func (s *Server) RequestShutdown(reason string) {
	select {
	case s.shutdown <- reason:
	default:
	}
}

// Run listens on the configured address and serves until shutdown.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves ln until shutdown, then drains and returns.
//
// This is the one shutdown path (invariant 17). All three ways in — a
// cancelled ctx, which is how SIGTERM arrives; the --timeout timer; and
// RequestShutdown, which is what the development route calls — land in the
// same select and leave through the same drain. Do not write a second one.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.logStart(ln.Addr())

	// The workers run under a context of their own so that the drain can end
	// them after the HTTP shutdown rather than at the same moment. workersDone
	// is closed when Run returns, which it does only once every worker has
	// finished the job it was holding or released its lease.
	workersCtx, stopWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWorkers()
	workersDone := make(chan struct{})
	if s.background == nil {
		close(workersDone)
	} else {
		go func() {
			defer close(workersDone)
			if err := s.background.Run(workersCtx); err != nil {
				s.log.Error("job workers stopped with an error", "error", err)
			}
		}()
	}

	if s.timeout > 0 {
		s.log.Info("shutdown timer armed", "timeout", s.timeout.String())
		t := s.afterFunc(s.timeout, func() { s.RequestShutdown(ReasonTimeout) })
		defer t.Stop()
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var reason string
	select {
	case err := <-serveErr:
		// The listener failed on its own. ErrServerClosed cannot appear here,
		// because nothing has called Shutdown yet.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		reason = ReasonSignal
	case reason = <-s.shutdown:
	}

	s.log.Info("shutting down", "reason", reason, "drain_timeout", s.drainTimeout.String())

	// The shutdown must outlive ctx: when the reason is a signal, ctx is
	// already cancelled, and a cancelled context would abandon in-flight
	// requests instead of draining them.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.drainTimeout)
	defer cancel()

	err := srv.Shutdown(drainCtx)
	if serr := <-serveErr; !errors.Is(serr, http.ErrServerClosed) {
		err = errors.Join(err, serr)
	}

	// The workers stop after the requests have drained, which is the order
	// DESIGN.md 11 gives and the only order that makes sense: a request still
	// in flight may still be enqueuing work. Then we wait, because "let
	// workers finish the current job or release its lease" is a promise that
	// is only kept if somebody waits for them to do it.
	stopWorkers()
	select {
	case <-workersDone:
	case <-drainCtx.Done():
		// The drain budget ran out with a job still in flight. Its lease
		// expires on its own and another worker takes it, which is the whole
		// reason the lease exists; saying so is better than looking hung.
		s.log.Warn("job workers did not stop within the drain timeout; their leases will expire",
			"drain_timeout", s.drainTimeout.String())
	}

	if err != nil {
		return fmt.Errorf("shutdown after %s: %w", reason, err)
	}

	s.log.Info("stopped", "reason", reason)
	return nil
}

// logStart writes the startup lines: the banner, then the structured record.
func (s *Server) logStart(bound net.Addr) {
	s.WriteBanner()

	s.log.Info("cmsd starting",
		"environment", s.env.String(),
		"addr", bound.String(),
		"routes", len(s.routes),
	)
	if !config.AddrIsLoopback(bound.String()) {
		s.log.Warn("listening on a non-loopback address; cmsd speaks plain HTTP and never terminates TLS",
			"addr", bound.String(),
		)
	}
}

// WriteBanner writes the plain-text startup banner.
//
// It is deliberately not only a log line. The design requires that
// "environment=production" be greppable in a production log (DESIGN.md 11,
// 14), and in production the structured log is JSON, where that string does not
// appear. An operator who greps a production log for "environment=" and finds
// "development" has found an incident; that has to work without knowing the log
// format. So the banner is written as text in both environments, and the
// structured record is written alongside it for everything that parses logs.
func (s *Server) WriteBanner() {
	if s.banner == nil {
		return
	}

	line := fmt.Sprintf("environment=%s", s.env)
	if s.resolution != nil {
		line = s.resolution.String()
	}
	fmt.Fprintln(s.banner, line)

	if s.env.IsDevelopment() {
		fmt.Fprintln(s.banner, "*** ENVIRONMENT=development: /__development/* routes are enabled.")
		fmt.Fprintln(s.banner, "*** Anyone who can reach this listener can log in as any user.")
	}
}
