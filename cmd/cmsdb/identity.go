// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
	"github.com/spf13/cobra"
)

// The subcommands that put a person and a set of roles into an empty database:
// "cmsdb bootstrap admin" and "cmsdb seed" (DESIGN.md 11).
//
// seed is separate from init so that a test can create an empty schema.

// newBootstrapCmd is the parent of "bootstrap admin". It exists as its own
// noun because the design names it that way and because the next thing to
// bootstrap will be a sibling rather than another flag.
func newBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the first administrator",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newBootstrapAdminCmd())
	return cmd
}

// newBootstrapAdminCmd creates the first administrator.
//
// Two rules, both from DESIGN.md 11, and both about the password:
//
//   - It never accepts a password as a command-line flag. Arguments are
//     visible in "ps" to every other user on the machine and they land in
//     shell history. There is no --password, and a test asserts there is not.
//   - It reads one from stdin, or generates one and prints it exactly once.
//     Printed once means printed once: nothing stores it, and a second run
//     will not print it again.
//
// It is idempotent in the direction that matters: a second run with the same
// email changes nothing and exits non-zero with a clear message. That is
// detected by the UNIQUE constraint on users.email, by result code
// (invariant 11), rather than by reading first and writing after -- which is a
// race, and which would also report success for a user somebody else created
// in between.
func newBootstrapAdminCmd() *cobra.Command {
	var (
		dir           string
		email         string
		name          string
		passwordStdin bool
	)
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Create the first administrator and print a generated password once",
		Long: "Create the first administrator.\n\n" +
			"The password is read from stdin with --password-stdin, or generated and\n" +
			"printed once. It is never accepted as a flag: arguments are visible in\n" +
			"\"ps\" and land in shell history.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			db, err := store.Open(ctx, dir, store.Options{})
			if err != nil {
				return err
			}
			defer db.Close()

			svc, err := service.New(db, service.Options{Clock: clock.Real{}})
			if err != nil {
				return err
			}

			password, generated, err := readPassword(cmd.InOrStdin(), passwordStdin)
			if err != nil {
				return err
			}

			u, err := svc.CreateUser(ctx, service.NewUser{
				Email:    email,
				Name:     name,
				Password: password,
			})
			if err != nil {
				if errors.Is(err, domain.ErrConflict) {
					// The message names what to do instead. "Already exists"
					// on its own sends somebody looking for a --force.
					return fmt.Errorf("a user with the email %q already exists; nothing was changed", email)
				}
				return err
			}

			// The admin role and its grant come from seed, which may not have
			// run. Running it here would make bootstrap do two things; asking
			// for it is one line the operator reads.
			if err := grantAdminRole(ctx, cmd.ErrOrStderr(), db, u); err != nil {
				return err
			}

			fmt.Fprintf(out, "created: %s <%s>\n", u.Name, u.Email)
			fmt.Fprintf(out, "uid: %s\n", u.UID)
			if generated {
				fmt.Fprintf(out, "password: %s\n", password)
				fmt.Fprintln(out, "This is the only time it is shown. Nothing stores it.")
			}
			return nil
		},
	}
	addDBFlag(cmd, &dir)
	cmd.Flags().StringVar(&email, "email", "", "the administrator's email address")
	cmd.Flags().StringVar(&name, "name", "", "the administrator's name")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false,
		"read the password from stdin instead of generating one")
	_ = cmd.MarkFlagRequired("email")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// grantAdminRole gives a freshly bootstrapped user the admin role, when seed
// has created one.
//
// A missing role is not an error: "cmsdb init" then "cmsdb bootstrap admin" is
// a legitimate order, and the operator is told to seed rather than refused.
// What must not happen is a silent no-op, which is why it says which it did.
func grantAdminRole(ctx context.Context, stderr io.Writer, db *store.DB, u domain.User) error {
	role, err := db.RoleBySlug(ctx, AdminRoleSlug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			fmt.Fprintf(stderr,
				"note: there is no %q role yet, so no role was assigned; run \"cmsdb seed\" and try again\n",
				AdminRoleSlug)
			return nil
		}
		return err
	}
	return db.AssignRole(ctx, u.ID, role.ID)
}

// readPassword returns the password to use and whether it was generated.
//
// The stdin path trims exactly one trailing newline, which is what "echo hunter2
// | cmsdb ..." produces, and nothing else: a password with a trailing space is
// a password, and silently trimming it makes a login fail with no explanation.
func readPassword(stdin io.Reader, fromStdin bool) (password string, generated bool, err error) {
	if !fromStdin {
		p, err := authz.GeneratePassword()
		return p, true, err
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		return "", false, fmt.Errorf("reading the password from stdin: %w", err)
	}
	p := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if p == "" {
		return "", false, fmt.Errorf("--password-stdin was given but stdin was empty")
	}
	return p, false, nil
}
