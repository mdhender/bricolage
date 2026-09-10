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
	var dir, file, output string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check integrity, foreign keys, leases, and orphaned resources",
		Long: "Check integrity, foreign keys, leases, and orphaned resources.\n\n" +
			"--db DIR checks DIR/cms.db, the database this system serves from.\n" +
			"--file FILE checks a database file under any name, which is what a\n" +
			"backup is: verifying one used to mean moving it into a directory\n" +
			"under the expected name first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (dir == "") == (file == "") {
				return errors.New("give exactly one of --db DIR and --file FILE")
			}

			// The one place outside internal/clock that reads the wall clock
			// is main, and this is one of them: a lease is judged expired
			// against an instant, and the instant is constructed here and
			// handed down (invariant 3).
			now := clock.Real{}.Now()

			// db stays nil for --file. A file under any name is read without
			// the DIR/cms.db convention and without opening a writer, and
			// nothing else this command does applies to one: reconciling
			// published_resources against an output tree is a question about
			// the live system rather than about a snapshot of it, which is why
			// --file and --output are mutually exclusive.
			var db *store.DB
			var report *store.CheckReport
			var err error
			if file != "" {
				report, err = store.CheckFile(cmd.Context(), file, now)
				if err != nil {
					return err
				}
			} else {
				db, err = store.Open(cmd.Context(), dir, store.Options{})
				if err != nil {
					return err
				}
				defer db.Close()

				report, err = db.Check(cmd.Context(), now)
				if err != nil {
					return err
				}
			}

			// The half of the check that is not in the database
			// (PLAN.md M9 acceptance 7). It needs the output tree, so it is
			// done here rather than inside store.Check, which reads rows and
			// opens no directories. Without --output the two lists stay empty
			// and the report says the question was not asked, because
			// "0 orphaned resources" from a check that never looked is the
			// most misleading line a report could carry.
			var reconciled bool
			if output != "" && db != nil {
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
			if report.QueueChecked {
				fmt.Fprintf(out, "stuck job leases: %d\n", report.StuckJobLeases)
			} else {
				// A schema older than migration 0008 has no queue. Reporting
				// zero would be a number that means "none" when it means "not
				// asked" (issue #25).
				fmt.Fprintln(out, "stuck job leases: not checked (this schema predates the queue)")
			}
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
	cmd.Flags().StringVar(&dir, "db", "", "directory holding cms.db; it must already exist")
	cmd.Flags().StringVar(&file, "file", "",
		"database file to check under any name, such as a backup; read-only")
	cmd.MarkFlagsMutuallyExclusive("db", "file")
	cmd.Flags().StringVar(&output, "output", "",
		"directory published files were written beneath; without it the output tree is not checked")
	cmd.MarkFlagsMutuallyExclusive("file", "output")
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
