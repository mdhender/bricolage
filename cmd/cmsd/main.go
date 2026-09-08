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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdhender/bricolage"
	"github.com/mdhender/bricolage/internal/buildenv"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/migrate"
	"github.com/mdhender/bricolage/internal/server"
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

	// db is the directory holding cms.db. It is on serve and not on routes,
	// because the route table does not depend on it and a flag that is parsed
	// and ignored is worse than no flag.
	db string
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
			srv, log, err := build(f)
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
			log.Info("database opened",
				"path", db.Path(),
				"user_version", db.SchemaVersion(),
				"migrations", migrate.Count(),
			)

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
			srv, _, err := build(f)
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
}

// build resolves the flags into a Server and the logger that goes with it. It
// is shared by serve and routes so that neither can drift from the other.
func build(f *flags) (*server.Server, *slog.Logger, error) {
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

	srv, err := server.New(server.Options{
		Environment: res.Environment,
		Addr:        f.addr,
		Timeout:     f.timeout,
		Logger:      log,
		Banner:      os.Stderr,
		Resolution:  &res,
	})
	if err != nil {
		return nil, nil, err
	}
	return srv, log, nil
}
