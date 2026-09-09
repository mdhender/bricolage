// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The assignment and queue commands (PLAN.md M5).
//
// earl is the acceptance-test harness for every milestone: everything M5 adds
// to the API is reachable from here, including clearing a due date, which is
// the operation nobody would think to build a flag for and which is exactly
// the one somebody needs at five o'clock.

// newDocAssignCmd is "earl doc assign UID --to USER [--due WHEN]", and its
// inverse.
//
// Unassigning is a flag on the same command rather than a command of its own,
// because "assign this to nobody" is the same decision as "assign this to
// Alice" and a person looking for it looks under assign.
func newDocAssignCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		to     string
		due    string
		nobody bool
	)
	cmd := &cobra.Command{
		Use:   "assign UID --to USER",
		Short: "Give a document to somebody, optionally with a due date",
		Long: "Give a document to somebody, optionally with a due date.\n\n" +
			"--to takes a user uid, or \"me\". --due takes a date (2026-03-01), a\n" +
			"timestamp (2026-03-01T17:00:00Z), or a duration from now (48h).\n\n" +
			"This does not check the document out, and it does not move it through\n" +
			"the workflow: who has a document and where it is in the process are\n" +
			"different questions.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			switch {
			case nobody && cmd.Flags().Changed("to"):
				return fmt.Errorf("--to and --nobody are two ways to say different things; give one")
			case nobody && cmd.Flags().Changed("due"):
				return fmt.Errorf("--nobody leaves the due date alone; use \"earl doc due\" to change it")
			case !nobody && to == "":
				return fmt.Errorf("give --to USER, or --nobody to return the document to the pile")
			}

			var doc documentResponse
			if nobody {
				if err := client.Do(cmd.Context(), http.MethodDelete,
					docPath(args[0])+"/assignment", nil, &doc); err != nil {
					return err
				}
				return printDoc(cmd.OutOrStdout(), doc, asJSON, "unassigned")
			}

			req := map[string]any{"user": to}
			if due != "" {
				req["due_at"] = due
			}
			if err := client.Do(cmd.Context(), http.MethodPost,
				docPath(args[0])+"/assignment", req, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, "assigned to "+doc.AssignedToName)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&to, "to", "", `the assignee's uid, or "me"`)
	cmd.Flags().StringVar(&due, "due", "", "when it is wanted: a date, a timestamp, or a duration from now")
	cmd.Flags().BoolVar(&nobody, "nobody", false, "return the document to the unassigned pile")
	return cmd
}

// newDocDueCmd sets or clears a deadline without touching the assignee.
//
// A deadline belongs to the work rather than to whoever is holding it, which
// is why it is not folded into assign: a story can have a date before it has a
// person, and putting it down does not make it less late.
func newDocDueCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		at     string
		clear  bool
	)
	cmd := &cobra.Command{
		Use:   "due UID --at WHEN",
		Short: "Set or clear a document's due date",
		Long: "Set or clear a document's due date.\n\n" +
			"--at takes a date (2026-03-01), a timestamp (2026-03-01T17:00:00Z), or\n" +
			"a duration from now (48h). --clear removes the deadline and leaves the\n" +
			"assignee alone.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			switch {
			case clear && cmd.Flags().Changed("at"):
				return fmt.Errorf("--at and --clear are two ways to say different things; give one")
			case !clear && at == "":
				return fmt.Errorf("give --at WHEN, or --clear to remove the deadline")
			}

			var doc documentResponse
			if clear {
				if err := client.Do(cmd.Context(), http.MethodDelete,
					docPath(args[0])+"/due", nil, &doc); err != nil {
					return err
				}
				return printDoc(cmd.OutOrStdout(), doc, asJSON, "due date cleared")
			}
			if err := client.Do(cmd.Context(), http.MethodPut,
				docPath(args[0])+"/due", map[string]any{"at": at}, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, "due "+formatDue(doc.DueAt))
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&at, "at", "", "when it is wanted: a date, a timestamp, or a duration from now")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the deadline")
	return cmd
}

// queueResponse is GET /api/v1/queues and GET /api/v1/queues/{slug}.
type queueResponse struct {
	Slug        string             `json:"slug"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	State       string             `json:"state"`
	Assignee    string             `json:"assignee"`
	Overdue     bool               `json:"overdue"`
	Count       int                `json:"count"`
	Documents   []documentResponse `json:"documents"`
}

// newQueueCmd is "earl queue [SLUG]" (DESIGN.md 11).
//
// With no slug it lists the saved definitions, which is the menu a person
// needs before they can name one. The definitions live in the server's
// configuration rather than in its schema, so asking the server is the only
// way to find out what it serves.
func newQueueCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "queue [SLUG]",
		Short: "Run a saved queue, or list the saved queues",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()

			if len(args) == 0 {
				var out struct {
					Queues []queueResponse `json:"queues"`
				}
				if err := client.Get(cmd.Context(), "/api/v1/queues", &out); err != nil {
					return err
				}
				if asJSON {
					return writeJSON(w, out)
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "SLUG\tNAME\tASKS")
				for _, q := range out.Queues {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", q.Slug, q.Name, describeQueue(q))
				}
				return tw.Flush()
			}

			var q queueResponse
			if err := client.Get(cmd.Context(), "/api/v1/queues/"+url.PathEscape(args[0]), &q); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(w, q)
			}
			fmt.Fprintf(w, "%s: %s\n", q.Name, describeQueue(q))
			return printDocTable(w, q.Documents)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// describeQueue says what question a queue asks, so that an empty one explains
// itself instead of looking broken.
func describeQueue(q queueResponse) string {
	var parts []string
	if q.State != "" {
		parts = append(parts, "in "+q.State)
	}
	switch q.Assignee {
	case "me":
		parts = append(parts, "assigned to you")
	case "nobody":
		parts = append(parts, "assigned to nobody")
	}
	if q.Overdue {
		parts = append(parts, "overdue")
	}
	if len(parts) == 0 {
		return "everything"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

// formatDue renders a deadline for a person. A document with none says so
// rather than printing a zero time that reads as the year 1.
func formatDue(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// printDocTable is the columns "earl doc list" and "earl queue" share.
//
// They share them deliberately: a queue is a saved question about the same
// rows, and a person reading one after the other should not have to find the
// state column in a different place.
func printDocTable(w io.Writer, docs []documentResponse) error {
	if len(docs) == 0 {
		fmt.Fprintln(w, "no documents")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UID\tKIND\tSTATE\tASSIGNED TO\tDUE\tCHECKED OUT BY\tUPDATED")
	for _, d := range docs {
		held := "-"
		if d.CheckedOutBy != "" {
			held = d.CheckedOutName
		}
		who := "-"
		if d.AssignedTo != "" {
			who = d.AssignedToName
		}
		due := formatDue(d.DueAt)
		if d.Overdue {
			due += " (overdue)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.UID, d.Kind, d.State, who, due, held, d.UpdatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

// listQuery builds the query string for "earl doc list" from the flags that
// were actually given.
//
// Only the flags a person passed are sent. A client that always sent
// "unassigned=false" would be a client whose default asked a different
// question from the one the server answers with no filters at all.
func listQuery(cmd *cobra.Command, f docListFlags) string {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(f.limit))
	if cmd.Flags().Changed("state") {
		q.Set("state", f.state)
	}
	if cmd.Flags().Changed("assignee") {
		q.Set("assignee", f.assignee)
	}
	if cmd.Flags().Changed("site") {
		q.Set("site", strconv.FormatInt(f.site, 10))
	}
	if f.unassigned {
		q.Set("unassigned", "true")
	}
	if f.overdue {
		q.Set("overdue", "true")
	}
	return "/api/v1/documents?" + q.Encode()
}

// docListFlags are the queue filters "earl doc list" accepts.
type docListFlags struct {
	state      string
	assignee   string
	site       int64
	unassigned bool
	overdue    bool
	limit      int
}
