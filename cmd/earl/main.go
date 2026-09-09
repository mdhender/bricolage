// Copyright (c) 2026 Michael D Henderson.

// Command earl exercises the API from the command line (DESIGN.md 11).
//
// It is a first-class client and the acceptance-test harness for every
// milestone, not a debug toy: if earl cannot do it, the API is incomplete.
//
// It points at the public origin rather than at the Go listener, so that it
// exercises the proxy hop that exists in production. Talking to
// 127.0.0.1:18443 directly bypasses the proxy and exercises a path that does
// not exist there (DESIGN.md 11).
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

const program = "earl"

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
		Short:         "Exercise the CMS API from the command line",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newLoginCmd(), newLogoutCmd(), newWhoamiCmd(), newAdminCmd(), newDocCmd(), newQueueCmd(), newJobCmd())
	root.AddCommand(newSiteCmd(), newCategoryCmd(), newChannelCmd(), newElementTypeCmd())
	root.AddCommand(newAlertCmd(), newNotificationCmd())
	root.AddCommand(newInviteCmd(), newUserCmd())
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
