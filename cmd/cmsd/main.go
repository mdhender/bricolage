// Copyright (c) 2026 Michael D Henderson.

// Command cmsd serves the REST API, the HTMX UI, and the background job
// workers. It speaks plain HTTP on loopback and never terminates TLS; a
// reverse proxy does that (DESIGN.md 11, invariant 15).
//
// It opens the database named by --db and verifies it, and it does nothing
// else to it: it never creates a directory, never creates a database, and
// never runs a migration (DESIGN.md 13.4, invariants 20 and 21). There is no
// --migrate flag and no --create flag, because there is no way to say yes.
//
// This file is flags and wiring. Behaviour lives in internal/server.
package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdhender/bricolage"
	"github.com/mdhender/bricolage/internal/buildenv"
	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/jobs"
	"github.com/mdhender/bricolage/internal/migrate"
	"github.com/mdhender/bricolage/internal/publish"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/server"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
	"github.com/spf13/cobra"
)

const program = "cmsd"

func main() {
	// Called explicitly, from main, never from init: main decides when the
	// check runs, because it may want to handle version or --help first
	// (invariant 18, DESIGN.md 14).
	buildenv.Verify()

	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", program, err)
		os.Exit(1)
	}
}

// flags are the resolved-from-the-command-line inputs. They are raw here;
// internal/config turns them into settings.
type flags struct {
	env     string
	addr    string
	timeout time.Duration

	// publicOrigin is the external origin a browser reaches this system at.
	// It is configuration and never inference: absolute URLs, the cookie, the
	// returnTo check, and the CSRF trusted-origin list all come from it, and
	// the Host header is attacker-controlled (DESIGN.md 11).
	publicOrigin string

	// trustedProxies are the networks X-Forwarded-* is honoured from
	// (invariant 14).
	trustedProxies []string

	// db is the directory holding cms.db. It is on serve and not on routes,
	// because the route table does not depend on it and a flag that is parsed
	// and ignored is worse than no flag.
	db string

	// workers is how many job workers run inside this process
	// (DESIGN.md 9, 11). It is on serve and not on routes for the reason db
	// is: the route table does not depend on it.
	//
	// Zero disables them, which is a supported configuration and not a
	// mistake: an operator running several cmsd processes behind one proxy
	// wants the workers in one of them, and a maintenance window wants them
	// in none.
	workers int

	// templates is the template tree and preview is the scratch tree
	// previews are written to (PLAN.md M8). Both are directories that must
	// already exist and neither has a default: a default would be a guess at
	// a path, and a guess that resolves to nothing looks exactly like a
	// template nobody wrote.
	//
	// Both are optional. M0 through M6 is a working editorial system with no
	// publishing, and a server started without them serves everything else
	// and answers 503 to a preview, naming the flag it was not given.
	templates string
	preview   string

	// output is the tree published files are written beneath (PLAN.md M9).
	// It is optional and it is never created: the root must already exist,
	// which is invariant 19's half of this that does not bend, so a mistyped
	// --output is a refusal at startup rather than a site published into a
	// directory nobody can find.
	//
	// What is created is the interior -- /features/film/2026/03/01/ -- which
	// is computed from a category path and a URI format rather than typed by
	// anybody. internal/publish/tree.go says why the exception stops exactly
	// there.
	output string

	// relatedFailure is what a publish does when a document it would have to
	// publish cannot be (PLAN.md M10, DESIGN.md 8.2). "fail" publishes
	// nothing, "warn" publishes what it can and reports the rest.
	//
	// It is a flag here because there is no config file reader yet;
	// DESIGN.md 8.2 spells it "publish.related_failure" and that is what
	// config.ParseRelatedFailure's refusal names, so the key and the flag say
	// the same words when the file arrives.
	relatedFailure string
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           program,
		Short:         "Serve the CMS API, the HTMX UI, and the job workers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var f flags
	root.AddCommand(newVersionCmd())
	root.AddCommand(newServeCmd(&f))
	root.AddCommand(newRoutesCmd(&f))
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version, commit, and Go version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), bricolage.VersionString(program))
			return nil
		},
	}
}

func newServeCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve until a signal, a timeout, or a shutdown request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := resolve(f)
			if err != nil {
				return err
			}

			// The database is opened and verified before anything binds a
			// port. Every failure in DESIGN.md 13.4 — a missing directory, a
			// missing cms.db, a wrong application_id, a schema version that is
			// not an exact match — returns here, with no listener and nothing
			// created. An operator runs "cmsdb migrate up" or fixes the path;
			// cmsd owns no way to repair it.
			db, err := store.Open(cmd.Context(), f.db, store.Options{})
			if err != nil {
				return err
			}
			defer db.Close()
			settings.log.Info("database opened",
				"path", db.Path(),
				"user_version", db.SchemaVersion(),
				"migrations", migrate.Count(),
			)

			// The template tree and the scratch tree, opened before anything
			// binds a port: a server that cannot read the directory it was
			// pointed at should say so while starting rather than on the
			// first preview. Neither is created if it is missing
			// (invariant 19).
			renderer, previews, err := settings.rendering(f)
			if err != nil {
				return err
			}
			if renderer == nil && previews == nil {
				settings.log.Info("rendering disabled; neither --templates nor --preview was given")
			} else {
				settings.log.Info("rendering",
					"templates", f.templates,
					"preview", f.preview,
					"reload", settings.resolution.Environment.IsDevelopment(),
				)
			}

			// The output tree, opened for the same reason and with the same
			// rule: it must already exist, and a --output that does not name
			// a directory is a refusal here rather than at the scheduled
			// hour.
			tree, err := settings.output(f)
			if err != nil {
				return err
			}
			defer func() { _ = tree.Close() }()

			// The one place outside internal/clock that reads the wall clock
			// is main, and this is it: the real clock is constructed here and
			// handed down (invariant 3).
			// The publisher, which needs a template tree to render with and
			// an output tree to write to. Without either this server
			// publishes nothing and says so, naming the flag: an editorial
			// system that renders previews and publishes nowhere is a
			// supported configuration (DESIGN.md 8.4).
			publisher, err := settings.publisher(db, renderer, tree)
			if err != nil {
				return err
			}
			// The cascade's policy, parsed before anything is logged about
			// publishing so that a misspelled word is a refusal while the
			// process is starting rather than the first time somebody
			// publishes a story with a related one.
			relatedFailure, err := config.ParseRelatedFailure(f.relatedFailure)
			if err != nil {
				return err
			}
			if publisher == nil {
				settings.log.Info("publishing disabled; --output and --templates are both needed")
			} else {
				settings.log.Info("publishing",
					"output", tree.Dir(), "related_failure", relatedFailure)
			}

			svc, err := service.New(db, service.Options{
				Clock:          clock.Real{},
				Logger:         settings.log,
				Renderer:       renderer,
				Preview:        previews,
				Publisher:      publisher,
				RelatedFailure: relatedFailure,
			})
			if err != nil {
				return err
			}

			// The workers. They are handed to the server rather than started
			// here, because they have to stop when it does and a second place
			// that decided when that was would be a second shutdown path
			// (invariant 17). --workers 0 hands it nothing and none run.
			// The alert dispatcher (PLAN.md M12). It reads the events
			// table after commit and turns matching rows into
			// notifications, so it runs wherever the workers do and stops
			// on the same one shutdown path (invariant 17). The e-mail
			// channel is the logging implementation, which is the default
			// this milestone ships: an installation that sends mail
			// supplies a Deliverer of its own here.
			dispatcher, err := events.NewDispatcher(events.DispatcherOptions{
				DB:     db,
				Clock:  clock.Real{},
				Logger: settings.log,
			})
			if err != nil {
				return err
			}

			var background server.Backgrounds
			if f.workers > 0 {
				// The registry is built here and the publish and expire kinds
				// are registered into it by the package that owns them. A
				// registry that knew how to build a publisher would be a
				// registry that imported half the system, and a kind
				// registered without its handler would be a name that lies
				// about what the queue can run (invariant 6).
				registry := jobs.NewRegistry()
				if publisher != nil {
					publisher.Register(registry)
				}
				pool, err := jobs.NewPool(jobs.PoolOptions{
					Queue:    svc.JobQueue(),
					Registry: registry,
					Workers:  f.workers,
					Logger:   settings.log,
				})
				if err != nil {
					return err
				}
				background = append(background, pool)
			} else {
				settings.log.Info("job workers disabled", "workers", f.workers)
			}
			background = append(background, dispatcher)

			srv, err := settings.server(svc, background)
			if err != nil {
				return err
			}

			// SIGTERM and SIGINT reach the same shutdown path as --timeout and
			// the development route (invariant 17).
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// The deferred Close is the tail of that one path: it runs after
			// Run has stopped accepting and drained, not on a second one.
			return srv.Run(ctx)
		},
	}
	addServeFlags(cmd, f)
	cmd.Flags().StringVar(&f.db, "db", "",
		"directory holding cms.db; it must already exist, and cmsd neither creates nor migrates it")
	_ = cmd.MarkFlagRequired("db")
	cmd.Flags().IntVar(&f.workers, "workers", jobs.DefaultWorkers,
		"background job workers to run in this process; 0 disables them")
	cmd.Flags().StringVar(&f.templates, "templates", "",
		"directory holding the template tree; it must already exist, and without it this server renders nothing")
	cmd.Flags().StringVar(&f.preview, "preview", "",
		"directory previews are written to; it must already exist, and without it this server serves none")
	cmd.Flags().StringVar(&f.output, "output", "",
		"directory published files are written beneath; it must already exist, and without it this server publishes nothing")
	cmd.Flags().StringVar(&f.relatedFailure, "related-failure", string(config.DefaultRelatedFailure),
		"what a publish does when a document it references cannot be published: fail or warn")
	return cmd
}

func newRoutesCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "routes",
		Short: "Print the route table this configuration produces and exit",
		Long: "Print the route table this configuration produces and exit.\n\n" +
			"This is the way to ask whether the /__development/* routes are\n" +
			"registered: they are absent from the table unless the resolved\n" +
			"environment is exactly \"development\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := resolve(f)
			if err != nil {
				return err
			}
			// No database, but the table still declares what "serve" would
			// mount: the question this command answers is "are the
			// /__development/* routes registered", and a table missing half
			// the server answers it wrongly. No workers either: the route
			// table does not depend on them, and this command exits.
			srv, err := settings.server(nil, nil)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, srv.Resolution())
			for _, r := range srv.Routes() {
				if r.Note == "" {
					fmt.Fprintln(out, r.Pattern)
					continue
				}
				fmt.Fprintf(out, "%s\t(%s)\n", r.Pattern, r.Note)
			}
			return nil
		},
	}
	addServeFlags(cmd, f)
	return cmd
}

// addServeFlags gives serve and routes the same flags, so that the table
// routes prints is the table serve would build.
//
// There is deliberately no --dev flag. Two switches meaning almost the same
// thing is how you end up with both of them set in production by somebody
// trying to make an error go away (DESIGN.md 11). The environment is the
// switch.
func addServeFlags(cmd *cobra.Command, f *flags) {
	cmd.Flags().StringVar(&f.env, "env", "",
		"environment: development or production (default production; also $CMS_ENV)")
	cmd.Flags().StringVar(&f.addr, "addr", config.DefaultAddr,
		"address to listen on; loopback, and never TLS")
	cmd.Flags().DurationVar(&f.timeout, "timeout", config.DefaultTimeout,
		"shut down gracefully after this duration; 0 means never")
	cmd.Flags().StringVar(&f.publicOrigin, "public-origin", config.DefaultPublicOrigin,
		"the external origin browsers reach this server at, through the proxy")
	cmd.Flags().StringSliceVar(&f.trustedProxies, "trusted-proxy", config.DefaultTrustedProxies,
		"CIDR blocks X-Forwarded-* headers are honoured from")
}

// settings are the flags after resolution: everything a Server needs, with
// nothing left to decide.
type settings struct {
	resolution config.Resolution
	log        *slog.Logger
	origin     config.PublicOrigin
	trusted    []*net.IPNet
	addr       string
	timeout    time.Duration
}

// resolve turns the flags into settings. It is shared by serve and routes so
// that the table routes prints is the table serve would build.
func resolve(f *flags) (*settings, error) {
	res := config.Resolve(config.Inputs{
		Flag: f.env,
		Env:  config.FromEnv(),
		// File: the config file is not read yet. --config arrives with the
		// settings that need it; a flag that is parsed and ignored is worse
		// than no flag.
	})

	log := config.NewLogger(res.Environment, os.Stderr)
	if !res.Recognized {
		log.Warn("unrecognized environment value; defaulted to production",
			"source", string(res.Source), "raw", res.Raw)
	}

	origin, err := config.ParsePublicOrigin(f.publicOrigin)
	if err != nil {
		return nil, err
	}
	trusted, err := config.ParseTrustedProxies(f.trustedProxies)
	if err != nil {
		return nil, err
	}

	return &settings{
		resolution: res,
		log:        log,
		origin:     origin,
		trusted:    trusted,
		addr:       f.addr,
		timeout:    f.timeout,
	}, nil
}

// rendering opens the template tree and the scratch tree, or returns nils.
//
// Both are optional and neither is created. A flag that was not given yields a
// nil, which the service reports as a 503 naming the flag; a flag that was
// given and does not name a directory is a hard failure, because an operator
// who asked for previews and got none silently would find out from a reader.
//
// The environment decides whether a template is re-read on every render
// (DESIGN.md 14). It is the only thing here the environment governs, and it
// governs no route.
func (s *settings) rendering(f *flags) (*render.Engine, *render.Scratch, error) {
	var (
		engine  *render.Engine
		scratch *render.Scratch
		err     error
	)
	if f.templates != "" {
		engine, err = render.New(render.Options{
			Root:   f.templates,
			Reload: s.resolution.Environment.IsDevelopment(),
		})
		if err != nil {
			return nil, nil, err
		}
	}
	if f.preview != "" {
		scratch, err = render.NewScratch(f.preview)
		if err != nil {
			return nil, nil, err
		}
	}
	return engine, scratch, nil
}

// output opens the output tree, or returns nil when --output was not given.
//
// The nil is a supported configuration and not a mistake: M0 through M8 is a
// working editorial system that renders previews and publishes nothing, and a
// request to publish on such a server is a 503 naming the flag.
func (s *settings) output(f *flags) (*publish.Tree, error) {
	if f.output == "" {
		return nil, nil
	}
	return publish.NewTree(f.output)
}

// publisher builds the publisher, or returns nil when this server cannot
// publish.
//
// It needs both trees. A template tree with no output tree renders previews
// and has nowhere to put a published page; an output tree with no template
// tree has somewhere to put bytes nothing can produce. Either half alone is a
// configuration that cannot publish, and saying so once at startup is better
// than a 503 per attempt with no explanation of which half is missing.
func (s *settings) publisher(db *store.DB, renderer *render.Engine, tree *publish.Tree) (*publish.Publisher, error) {
	if renderer == nil || tree == nil {
		return nil, nil
	}
	return publish.New(publish.Options{
		DB:       db,
		Renderer: renderer,
		Tree:     tree,
		Clock:    clock.Real{},
		Logger:   s.log,
	})
}

// server builds the Server. A nil service is the "cmsd routes" case: the table
// declares what serve would mount, with handlers that refuse. A nil background
// is a server that runs no job workers.
func (s *settings) server(svc *service.Service, background server.Background) (*server.Server, error) {
	return server.New(server.Options{
		Environment:                 s.resolution.Environment,
		Addr:                        s.addr,
		Timeout:                     s.timeout,
		Logger:                      s.log,
		Banner:                      os.Stderr,
		Resolution:                  &s.resolution,
		Service:                     svc,
		DeclareRoutesWithoutService: svc == nil,
		Origin:                      s.origin,
		TrustedProxies:              s.trusted,
		Background:                  background,
	})
}
