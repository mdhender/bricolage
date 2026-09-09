// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/jobs"
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

// DefaultElementTypeKey is the element type seed creates (DESIGN.md 5.2).
//
// Every document points at one, so without it "earl doc create" has nothing to
// name and a freshly seeded database cannot hold a document. Its schema is an
// empty field list: M3 stores the schema and does not yet validate content
// against it (PLAN.md M3, "Schema"), and an empty declaration is the honest
// starting point rather than a set of fields nobody asked for.
const (
	DefaultElementTypeKey    = "story"
	DefaultElementTypeName   = "Story"
	DefaultElementTypeSchema = `{"fields":[]}`
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
// The default story workflow is not created here. It is created by the
// migration that adds the workflow tables, because documents.workflow_id is
// NOT NULL with a composite foreign key to workflow_states: no document row
// may exist before a workflow does, and a database that already holds
// documents has nowhere to put them otherwise
// (internal/migrate/schema/0005_workflow.sql). DESIGN.md 5.4 says the default
// story workflow is seeded by "cmsdb", and "cmsdb init" is cmsdb. What seed
// does is report it, so that "what does a fresh database contain" is still one
// command's output -- and so that the state machine is written down once
// rather than twice.
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

			if err := seed(ctx, db, out); err != nil {
				return err
			}
			if demo {
				return seedDemo(ctx, db, out)
			}
			return nil
		},
	}
	addDBFlag(cmd, &dir)
	cmd.Flags().BoolVar(&demo, "demo", false,
		"also seed one sample document and one queued job; needs \"cmsdb bootstrap admin\" to have run")
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

	switch et, err := db.ElementTypeByKeyName(ctx, DefaultElementTypeKey); {
	case err == nil:
		fmt.Fprintf(out, "element type: %s (already present)\n", et.KeyName)
	case errors.Is(err, domain.ErrNotFound):
		uid, err := ids.New(now)
		if err != nil {
			return err
		}
		et, err := db.CreateElementType(ctx, store.NewElementType{
			UID:       uid,
			KeyName:   DefaultElementTypeKey,
			Name:      DefaultElementTypeName,
			Kind:      domain.KindStory,
			TopLevel:  true,
			Schema:    DefaultElementTypeSchema,
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "element type: %s (%s)\n", et.KeyName, et.Name)
	default:
		return err
	}

	workflows, err := db.ListWorkflows(ctx)
	if err != nil {
		return err
	}
	if len(workflows) == 0 {
		return fmt.Errorf("this database has no workflow; the migration that creates the workflow tables seeds the default one, so a database with none has been edited by hand")
	}
	for _, w := range workflows {
		fmt.Fprintf(out, "workflow: %s (%s, %s documents, starts in %q, %d states, %d transitions)\n",
			w.UID, w.Name, w.Kind, w.InitialState, len(w.States), len(w.Transitions))
	}

	fmt.Fprintln(out, "seeded")
	return nil
}

// demoTitle is the sample document --demo creates. It is one document rather
// than a library: the flag exists so that somebody can see the editorial cycle
// working, and a second sample teaches nothing the first did not.
const demoTitle = "The Quick Brown Fox"

// seedDemo creates one sample document, checked in at version 1.
//
// It needs somebody to attribute the content to, because
// document_versions.created_by is a real foreign key and there is no system
// user. That makes "cmsdb bootstrap admin" a prerequisite, and the refusal
// says so rather than inventing an account.
func seedDemo(ctx context.Context, db *store.DB, out io.Writer) error {
	now := clock.Real{}.Now()

	author, err := db.FirstUser(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("--demo has nobody to attribute the sample content to; run \"cmsdb bootstrap admin\" first")
		}
		return err
	}

	// Idempotent by title: a second run reports what is already there rather
	// than stacking copies, which is the same promise the rest of seed makes.
	existing, err := db.QueryDocuments(ctx, store.DocumentQuery{
		Filter: domain.DocumentFilter{Limit: 1000},
	})
	if err != nil {
		return err
	}
	for _, d := range existing {
		v, err := db.VersionByNumber(ctx, d.ID, 1)
		if err != nil {
			return err
		}
		if v.Title == demoTitle {
			fmt.Fprintf(out, "demo: %s (already present, %s)\n", demoTitle, d.UID)
			// The job is still offered: --demo seeds two things, and a run
			// that found the document already there must not skip the half
			// that is not. Its own idempotency check is below.
			return seedDemoJob(ctx, db, out, author.ID, now)
		}
	}

	site, err := db.SiteByDomain(ctx, DefaultSiteDomain)
	if err != nil {
		return err
	}
	et, err := db.ElementTypeByKeyName(ctx, DefaultElementTypeKey)
	if err != nil {
		return err
	}
	// The workflow the sample document enters. Resolving it rather than
	// assuming one is the same rule the service follows: a document with no
	// process is not a document this schema can hold.
	wf, err := db.WorkflowFor(ctx, domain.KindStory, site)
	if err != nil {
		return err
	}
	uid, err := ids.New(now)
	if err != nil {
		return err
	}

	doc, _, err := db.CreateDocument(ctx, store.NewDocument{
		UID:           uid,
		SiteID:        site,
		Kind:          domain.KindStory,
		ElementTypeID: et.ID,
		WorkflowID:    wf.ID,
		State:         wf.InitialState,
		Title:         demoTitle,
		Slug:          "quick-brown-fox",
		Content:       `{"body":"The quick brown fox jumps over the lazy dog."}`,
		CreatedBy:     author.ID,
		CreatedAt:     now,
		Event: domain.Event{
			Type:    events.DocumentCreated,
			ActorID: author.ID,
			Payload: map[string]any{
				"uid": uid, "title": demoTitle, "seed": true,
				"workflow": wf.Name, "state": wf.InitialState,
			},
			OccurredAt: now,
		},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "demo: %s (%s, an open working draft in %q)\n", demoTitle, doc.UID, doc.State)

	return seedDemoJob(ctx, db, out, author.ID, now)
}

// seedDemoJob puts one noop job on the queue (PLAN.md M6).
//
// The queue is the one part of this system nothing else can show you. A
// document you can create from earl; a job you cannot, deliberately -- work is
// scheduled by the operation that needs it, and until M9 no operation needs
// any. So --demo, whose whole purpose is that somebody can see the system
// working, seeds one, and "cmsd serve --workers 1" then runs it while you
// watch. It is also what M6's end-to-end test drives.
//
// It is enqueued through the store rather than through internal/jobs, for the
// reason everything in cmsdb is: this command owns no service, and the uid and
// the instant are minted here from the real clock and handed down
// (invariant 3).
func seedDemoJob(ctx context.Context, db *store.DB, out io.Writer, authorID int64, now time.Time) error {
	// Idempotent, like the rest of seed: a second run reports what is there
	// rather than stacking copies. Any noop job at all is enough to say the
	// queue has been seeded -- the point is that there is something to watch,
	// not that there is exactly one of it.
	existing, err := db.QueryJobs(ctx, domain.JobFilter{Kind: jobs.KindNoop, Limit: 1})
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		fmt.Fprintf(out, "demo: a %s job is already queued (%s)\n", jobs.KindNoop, existing[0].UID)
		return nil
	}

	uid, err := ids.New(now)
	if err != nil {
		return err
	}
	job := domain.NewJob{
		Kind: jobs.KindNoop,
		// Bulk, because it is a demonstration and nothing is waiting for it.
		// Every bulk operation defaults to priority 5 (DESIGN.md 9), and a
		// sample that took the urgent end of the scale would teach the wrong
		// habit to whoever copies it.
		Priority:  domain.PriorityBulk,
		CreatedBy: authorID,
	}.Normalize(now)

	seeded, err := db.EnqueueJob(ctx, store.NewJob{
		UID: uid,
		Job: job,
		Event: domain.Event{
			Type:    events.JobEnqueued,
			ActorID: authorID,
			Payload: map[string]any{
				"uid": uid, "kind": job.Kind, "priority": job.Priority, "seed": true,
			},
			OccurredAt: now,
		},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "demo: one %s job queued (%s); run \"cmsd serve --workers 1\" to watch it run\n",
		seeded.Kind, seeded.UID)
	return nil
}
