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

// DefaultOutputChannelName is the output channel seed creates (DESIGN.md 5.3).
//
// A site with no output channel has no answer to "what is this document's
// address", which is the question M7 exists to answer, so a seeded system with
// none would be a system where the milestone's own command prints a refusal.
// One channel, the web, with the URI format a newsroom expects: the section,
// the date, the slug.
//
// use_slug is on. It is off in the schema's DEFAULT because a channel that
// does not use one is a legitimate configuration; it is on here because a URI
// format ending in %{slug} with slugs turned off produces the same address for
// every story published on a given day, and a starting point that collides is
// not a starting point.
const (
	DefaultOutputChannelName = "Web"
)

// DefaultElementTypeKey is the element type seed creates (DESIGN.md 5.2).
//
// Every document points at one, so without it "earl doc create" has nothing to
// name and a freshly seeded database cannot hold a document.
//
// Its schema declares two fields as of M7, where M3 declared none. The reason
// is that M7 validates a check-in against the schema (PLAN.md M7
// acceptance 5), and an element type declaring no fields declares that a
// document of that type carries none -- which would refuse every check-in
// carrying a body. An empty field list was the honest starting point while
// nothing read it; a starting point that refuses the first thing anybody does
// is not.
//
// No field is required, deliberately. "cmsdb seed" produces a system somebody
// is about to explore, and a required field turns "earl doc create --title X"
// followed by a check-in into a refusal before they have seen anything work.
// An installation that wants one says so with "earl element-type update".
//
// The third field is M10's. "related" is a repeatable document field, which is
// what the related-asset cascade reads: publishing a story publishes the
// stories it names, each with its own pinned version (DESIGN.md 8.2). It is
// seeded rather than left for an administrator for the same reason "body" is
// -- an element type declaring no document field declares that a story cannot
// reference one, and a starting point where the milestone's own feature is
// unreachable is not a starting point.
const (
	DefaultElementTypeKey    = "story"
	DefaultElementTypeName   = "Story"
	DefaultElementTypeSchema = `{"fields":[{"name":"body","type":"block"},{"name":"deck","type":"text"},` +
		`{"name":"related","type":"document","repeatable":true}]}`
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

	site, err := db.SiteByDomain(ctx, DefaultSiteDomain)
	if err != nil {
		return err
	}

	// The root category is created in the same transaction as its site, by
	// store.CreateSite, and for sites that predate M7 by migration 0009. Seed
	// reports it rather than creating it, the way it reports the workflow: a
	// thing written down twice is a thing that drifts.
	root, err := db.CategoryByPath(ctx, site, domain.RootPath)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("site %d has no root category; every site gets one when it is created, so a site with none has been edited by hand", site)
		}
		return err
	}
	fmt.Fprintf(out, "category: %s (%s, the root of %s)\n", root.Path, root.Name, DefaultSiteDomain)

	switch oc, err := db.OutputChannelByName(ctx, site, DefaultOutputChannelName); {
	case err == nil:
		fmt.Fprintf(out, "output channel: %s (already present, %s)\n", oc.Name, oc.URIFormat)
	case errors.Is(err, domain.ErrNotFound):
		uid, err := ids.New(now)
		if err != nil {
			return err
		}
		oc, err := db.CreateOutputChannel(ctx, domain.OutputChannel{
			UID:            uid,
			SiteID:         site,
			Name:           DefaultOutputChannelName,
			Protocol:       domain.DefaultProtocol,
			Filename:       domain.DefaultFilename,
			FileExt:        domain.DefaultFileExt,
			URIFormat:      domain.DefaultURIFormat,
			FixedURIFormat: domain.DefaultFixedURIFormat,
			UseSlug:        true,
			URICase:        domain.URICaseLower,
		}, domain.Event{})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "output channel: %s (%s)\n", oc.Name, oc.URIFormat)
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
		// A cover date, because the default output channel's URI format
		// carries %Y/%m/%d and a document with none has no address to show
		// (domain.BuildURI). The sample exists so that somebody can watch the
		// system work, and "earl doc uris" is half of what M7 added.
		CoverDate: now.UTC().Format("2006-01-02"),
		Content:   `{"body":"The quick brown fox jumps over the lazy dog."}`,
		CreatedBy: author.ID,
		CreatedAt: now,
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
	// Filed in the site's root, so that the sample has an address. A document
	// filed nowhere has none, which is the honest answer and a poor
	// demonstration.
	root, err := db.CategoryByPath(ctx, site, domain.RootPath)
	if err != nil {
		return err
	}
	if _, err := db.SetDocumentCategories(ctx, doc.ID, []int64{root.ID}, now, domain.Event{
		Type:    events.DocumentFiled,
		ActorID: author.ID,
		Payload: map[string]any{
			"uid": doc.UID, "primary": root.Path, "categories": []string{root.Path}, "seed": true,
		},
		OccurredAt: now,
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "demo: %s (%s, an open working draft in %q, filed at %s)\n",
		demoTitle, doc.UID, doc.State, root.Path)

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
