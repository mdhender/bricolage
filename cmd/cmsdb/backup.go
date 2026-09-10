// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"errors"
	"fmt"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/migrate"
	"github.com/mdhender/bricolage/internal/store"
	"github.com/spf13/cobra"
)

// newBackupCmd takes a consistent snapshot of a live database (#10).
//
// It exists rather than a documented "sqlite3 .backup" line because cmsdb is
// already on the server, already knows a database is DIR/cms.db, and already
// refuses to open one whose application_id is not CMS0. sqlite3 inherits none
// of that: pointed at the wrong directory it writes a zero-byte file and exits
// 0, and it is not installed on the host anyway.
//
// There is no restore command, on purpose. Restoring is a cp with the service
// stopped, written out by a person who has stopped to think; the safe
// direction is the one that gets automated.
func newBackupCmd() *cobra.Command {
	var dir, to string
	var overwrite bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write a consistent snapshot of the database to a file",
		Long: "Write a consistent snapshot of the database to a file.\n\n" +
			"Runs against a live database: the server does not have to be stopped,\n" +
			"and the snapshot is taken through the write-ahead log rather than\n" +
			"around it. The file comes out compacted and is verified before it is\n" +
			"given the name asked for.\n\n" +
			"The directory holding --to must already exist. Nothing in this system\n" +
			"creates a directory, and a backup command is no exception.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// AnyVersion: a backup is a copy of a file, not an interpretation
			// of its rows, so the application ID is the only thing it has to
			// agree about (issue #25). Requiring the version to match made
			// this command refuse the database the deploy procedure exists to
			// back up, because on a deploy carrying a migration the new binary
			// and the old database disagree by construction.
			db, err := store.Open(cmd.Context(), dir, store.Options{Version: store.AnyVersion})
			if err != nil {
				return err
			}
			defer db.Close()

			// The one place outside internal/clock that reads the wall clock
			// is main, and this is one of them: the backup is verified, and a
			// lease is judged expired against an instant (invariant 3).
			report, err := db.Backup(cmd.Context(), to, store.BackupOptions{
				Overwrite: overwrite,
				Now:       clock.Real{}.Now(),
			})
			// The refusal to overwrite is the one an operator meets by
			// accident, on the second deploy of a day, so it names the flag
			// that permits it rather than leaving them to find it in --help.
			var exists *store.ExistsError
			if errors.As(err, &exists) {
				return fmt.Errorf("%w; pass --overwrite to replace it", err)
			}
			if err != nil {
				return err
			}

			// The path, the size, and the version, so that the line in a
			// deploy log is evidence rather than reassurance.
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "backup: %s\n", report.Path)
			fmt.Fprintf(out, "size: %d bytes (from %s)\n", report.Size, db.Path())
			fmt.Fprintf(out, "application_id: %s\n", migrate.AppIDString(report.Check.AppID))
			fmt.Fprintf(out, "user_version: %d (this binary embeds %d migrations)\n",
				report.Check.SchemaVersion, report.Check.Migrations)
			fmt.Fprintln(out, "verified")
			return nil
		},
	}
	addDBFlag(cmd, &dir)
	cmd.Flags().StringVar(&to, "to", "",
		"file to write the backup to; the directory holding it must already exist")
	_ = cmd.MarkFlagRequired("to")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false,
		"replace --to if it is already there; without this an existing file is refused")
	return cmd
}
