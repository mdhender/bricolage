// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/migrate"
	"github.com/mdhender/bricolage/internal/publish"
	"github.com/mdhender/bricolage/internal/store"
	"github.com/spf13/cobra"
)

// newInitCmd is the one subcommand permitted to create a database file
// (invariant 20). It creates no directory, and running it twice is safe.
func newInitCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create DIR/cms.db and apply every migration",
		Long: "Create DIR/cms.db and apply every migration.\n\n" +
			"DIR must already exist: nothing in this system creates a directory,\n" +
			"because a mistyped path that creates one leaves an empty CMS that\n" +
			"looks exactly like the real one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			db, err := store.Create(ctx, dir)
			var exists *store.ExistsError
			if errors.As(err, &exists) {
				// Running init twice is safe and says so. Reporting the state
				// it found is more use than reporting that nothing happened.
				db, err := store.Open(ctx, dir, store.Options{Version: store.AllowBehind})
				if err != nil {
					return err
				}
				defer db.Close()
				fmt.Fprintf(out, "already initialised: %s\n", db.Path())
				return describe(out, db)
			}
			if err != nil {
				return err
			}
			defer db.Close()

			fmt.Fprintf(out, "initialised: %s\n", db.Path())
			return describe(out, db)
		},
	}
	addDBFlag(cmd, &dir)
	return cmd
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Show or apply schema migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newMigrateStatusCmd(), newMigrateUpCmd())
	return cmd
}

func newMigrateStatusCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List applied and pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// AllowBehind: reporting that migrations are pending is the whole
			// job, so a pending migration is not an error here.
			db, err := store.Open(cmd.Context(), dir, store.Options{Version: store.AllowBehind})
			if err != nil {
				return err
			}
			defer db.Close()

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "database: %s\n", db.Path())
			if err := describe(out, db); err != nil {
				return err
			}
			fmt.Fprint(out, migrate.Describe(db.SchemaVersion()))
			return nil
		},
	}
	addDBFlag(cmd, &dir)
	return cmd
}

func newMigrateUpCmd() *cobra.Command {
	var (
		dir string
		to  int
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := store.Open(cmd.Context(), dir, store.Options{Version: store.AllowBehind})
			if err != nil {
				return err
			}
			defer db.Close()

			before, after, err := db.Migrate(cmd.Context(), to)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "database: %s\n", db.Path())
			if before == after {
				fmt.Fprintf(out, "nothing to do: user_version %d\n", after)
			} else {
				fmt.Fprintf(out, "migrated: user_version %d -> %d\n", before, after)
			}
			fmt.Fprint(out, migrate.Describe(after))
			return nil
		},
	}
	addDBFlag(cmd, &dir)
	cmd.Flags().IntVar(&to, "to", -1,
		"stop after this migration; the default applies every embedded migration")
	return cmd
}

func newCheckCmd() *cobra.Command {
	var dir, output string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check integrity, foreign keys, leases, and orphaned resources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := store.Open(cmd.Context(), dir, store.Options{})
			if err != nil {
				return err
			}
			defer db.Close()

			// The one place outside internal/clock that reads the wall clock
			// is main, and this is one of them: a lease is judged expired
			// against an instant, and the instant is constructed here and
			// handed down (invariant 3).
			report, err := db.Check(cmd.Context(), clock.Real{}.Now())
			if err != nil {
				return err
			}

			// The half of the check that is not in the database
			// (PLAN.md M9 acceptance 7). It needs the output tree, so it is
			// done here rather than inside store.Check, which reads rows and
			// opens no directories. Without --output the two lists stay empty
			// and the report says the question was not asked, because
			// "0 orphaned resources" from a check that never looked is the
			// most misleading line a report could carry.
			var reconciled bool
			if output != "" {
				tree, err := publish.NewTree(output)
				if err != nil {
					return err
				}
				defer func() { _ = tree.Close() }()

				diff, err := publish.Reconcile(cmd.Context(), db, tree)
				if err != nil {
					return err
				}
				report.MissingFiles = diff.Missing
				report.UnknownFiles = diff.Unknown
				report.OrphanedResources = diff.Count()
				reconciled = true
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "database: %s\n", report.Path)
			fmt.Fprintf(out, "application_id: %s\n", migrate.AppIDString(report.AppID))
			fmt.Fprintf(out, "user_version: %d (this binary embeds %d migrations)\n",
				report.SchemaVersion, report.Migrations)
			for _, v := range report.ForeignKeyViolations {
				fmt.Fprintf(out, "foreign key: %s\n", v)
			}
			for _, p := range report.IntegrityProblems {
				fmt.Fprintf(out, "integrity: %s\n", p)
			}
			// A stuck lease is reported and does not fail the check: it is
			// what a worker that died looks like from the outside, and the
			// queue recovers on its own when the lease expires.
			fmt.Fprintf(out, "stuck job leases: %d\n", report.StuckJobLeases)
			if !reconciled {
				fmt.Fprintln(out, "orphaned resources: not checked (give --output DIR)")
			} else {
				fmt.Fprintf(out, "orphaned resources: %d\n", report.OrphanedResources)
				for _, p := range report.MissingFiles {
					fmt.Fprintf(out, "missing file: %s (a resource row names it and it is not there)\n", p)
				}
				for _, p := range report.UnknownFiles {
					fmt.Fprintf(out, "unknown file: %s (nothing claims it, so nothing will expire it)\n", p)
				}
			}

			if !report.OK() {
				// A check that finds damage and exits 0 is a check nobody can
				// put in a cron job.
				return fmt.Errorf("check found %d foreign key violations, %d integrity problems, and %d orphaned resources",
					len(report.ForeignKeyViolations), len(report.IntegrityProblems), report.OrphanedResources)
			}
			fmt.Fprintln(out, "ok")
			return nil
		},
	}
	addDBFlag(cmd, &dir)
	cmd.Flags().StringVar(&output, "output", "",
		"directory published files were written beneath; without it the output tree is not checked")
	return cmd
}

func newVacuumCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "vacuum",
		Short: "Rebuild the database file, reclaiming space",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := store.Open(cmd.Context(), dir, store.Options{})
			if err != nil {
				return err
			}
			defer db.Close()

			if err := db.Vacuum(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "vacuumed: %s\n", db.Path())
			return nil
		},
	}
	addDBFlag(cmd, &dir)
	return cmd
}

// describe prints the two markers this system keeps, which is what every
// subcommand that opens a database reports before doing anything else. They
// are the answer to "which database is this and is it the one this binary
// expects".
func describe(out io.Writer, db *store.DB) error {
	_, err := fmt.Fprintf(out, "application_id: %s\nuser_version: %d (this binary embeds %d migrations)\n",
		migrate.AppIDString(migrate.AppID), db.SchemaVersion(), migrate.Count())
	return err
}
