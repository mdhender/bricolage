// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
	"github.com/spf13/cobra"
)

// The role slugs seed creates. They are constants because "cmsdb bootstrap
// admin" names one of them and because a typo in a role slug is a user with no
// permissions and no error.
const (
	AdminRoleSlug  = "admin"
	EditorRoleSlug = "editor"
	WriterRoleSlug = "writer"
	ViewerRoleSlug = "viewer"
)

// DefaultSiteDomain is the site seed creates, matching the development public
// origin so that a freshly seeded database and a freshly started server agree
// about what host they are.
const (
	DefaultSiteName   = "Default"
	DefaultSiteDomain = "htmx-app.localhost"
)

// seedRole is one role and the single grant it carries.
//
// Every role here is granted globally -- the scope constrains nothing -- which
// is right for a starting point and wrong as a destination: the interesting
// grants are per-site and per-category, and they are written by an
// administrator through the anti-escalation check (DESIGN.md 7.3), not seeded.
//
// The scale is the point. An editor holds Create, which subsumes Read, Edit
// and Recall, because the scale is ordered and cumulative; only the
// administrator holds Publish.
var seedRoles = []struct {
	Slug      string
	Name      string
	Privilege domain.Privilege
}{
	{AdminRoleSlug, "Administrator", domain.Publish},
	{EditorRoleSlug, "Editor", domain.Create},
	{WriterRoleSlug, "Writer", domain.Edit},
	{ViewerRoleSlug, "Viewer", domain.Read},
}

// newSeedCmd creates the roles, their grants, and one site (DESIGN.md 11).
//
// It is separate from init so that a test can create an empty schema, and it
// is idempotent: every insert is guarded by the UNIQUE constraint it would
// violate, detected by result code (invariant 11), so a second run reports
// what was already there rather than failing or duplicating it.
//
// The default workflow the design also asks of seed arrives in M4, with the
// workflow tables. Seeding a workflow into a schema that has no workflow_states
// is not possible, and naming it here without creating it is the "rule column
// nothing reads" that invariant 6 is about.
func newSeedCmd() *cobra.Command {
	var (
		dir  string
		demo bool
	)
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Create the default roles, their grants, and one site",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			db, err := store.Open(ctx, dir, store.Options{})
			if err != nil {
				return err
			}
			defer db.Close()

			if demo {
				// --demo seeds sample content, which needs documents. It is
				// accepted and refused rather than ignored: a flag that is
				// parsed and does nothing is worse than one that is not there.
				return fmt.Errorf("--demo has nothing to seed until documents exist (PLAN.md M3)")
			}

			return seed(ctx, db, out)
		},
	}
	addDBFlag(cmd, &dir)
	cmd.Flags().BoolVar(&demo, "demo", false, "also seed sample content")
	return cmd
}

// seed does the work, so that a test can call it without a cobra command.
func seed(ctx context.Context, db *store.DB, out io.Writer) error {
	// The clock is read here, in main, and handed down (invariant 3).
	now := clock.Real{}.Now()

	for _, r := range seedRoles {
		role, err := db.CreateRole(ctx, r.Slug, r.Name)
		switch {
		case err == nil:
			fmt.Fprintf(out, "role: %s (%s)\n", role.Slug, role.Name)
		case errors.Is(err, domain.ErrConflict):
			if role, err = db.RoleBySlug(ctx, r.Slug); err != nil {
				return err
			}
			fmt.Fprintf(out, "role: %s (already present)\n", role.Slug)
		default:
			return err
		}

		// The grant is written only when the role carries none, so that a
		// second run does not stack duplicates and an administrator's changes
		// to a seeded role are not undone by re-seeding.
		existing, err := db.GrantsForRole(ctx, role.ID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			continue
		}
		g, err := db.CreateGrant(ctx, domain.Grant{
			RoleID:    role.ID,
			Privilege: r.Privilege,
			CreatedAt: now,
			// CreatedBy is zero: the system wrote it, before there was
			// anybody to have written it.
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "grant: %s may %s over %s\n", role.Slug, g.Privilege, g.Scope)
	}

	switch id, err := db.SiteByDomain(ctx, DefaultSiteDomain); {
	case err == nil:
		fmt.Fprintf(out, "site: %s (already present, id %d)\n", DefaultSiteDomain, id)
	case errors.Is(err, domain.ErrNotFound):
		uid, err := ids.New(now)
		if err != nil {
			return err
		}
		id, err := db.CreateSite(ctx, store.NewSite{
			UID:    uid,
			Name:   DefaultSiteName,
			Domain: DefaultSiteDomain,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "site: %s (%s, id %d)\n", DefaultSiteDomain, DefaultSiteName, id)
	default:
		return err
	}

	fmt.Fprintln(out, "seeded")
	return nil
}
