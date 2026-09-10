// Copyright (c) 2026 Michael D Henderson.

// Command cmsdb initialises, migrates, bootstraps, seeds, and checks the
// database (DESIGN.md 11).
//
// --db names a directory that must already exist, and the database inside it
// is always cms.db. cmsdb never creates a directory: a missing DIR is a hard
// failure naming the directory, in every subcommand including init
// (invariant 19). init is the only subcommand permitted to create the database
// file, and cmsdb is the only command permitted to migrate one (invariant 20).
//
// This file is flags and wiring. Behaviour lives in internal/.
package main

import (
	"fmt"
	"os"

	"github.com/mdhender/bricolage"
	"github.com/mdhender/bricolage/internal/buildenv"
	"github.com/spf13/cobra"
)

const program = "cmsdb"

func main() {
	// Called explicitly, from main, never from init (invariant 18).
	buildenv.Verify()

	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", program, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           program,
		Short:         "Create, migrate, and check the CMS database",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version, commit, and Go version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), bricolage.VersionString(program))
			return nil
		},
	})
	root.AddCommand(
		newInitCmd(),
		newMigrateCmd(),
		newBootstrapCmd(),
		newSeedCmd(),
		newCheckCmd(),
		newBackupCmd(),
		newVacuumCmd(),
	)
	return root
}

// addDBFlag gives a subcommand the --db flag, which every subcommand has and
// none may do without.
//
// It names a directory, never a file. The database inside it is the constant
// cms.db, so --db cannot address two different files depending on which
// subcommand was typed (DESIGN.md 13.1).
func addDBFlag(cmd *cobra.Command, dir *string) {
	cmd.Flags().StringVar(dir, "db", "", "directory holding cms.db; it must already exist")
	_ = cmd.MarkFlagRequired("db")
}
