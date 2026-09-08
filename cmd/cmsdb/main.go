// Copyright (c) 2026 Michael D Henderson.

// Command cmsdb initialises, migrates, bootstraps, seeds, and checks the
// database (DESIGN.md 11).
//
// In M0 it prints its version and nothing else. The subcommands arrive in M1,
// under the rules in DESIGN.md 13.4: --db names a directory that must already
// exist, the database inside it is always cms.db, and init is the only
// subcommand permitted to create it.
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
	return root
}
