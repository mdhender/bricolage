// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The document commands (PLAN.md M3).
//
// earl is the acceptance-test harness for every milestone, not a debug toy: if
// earl cannot do it, the API is incomplete. Everything M3 adds to the API is
// reachable from here, including the diff, whose output is golden-tested.

// documentResponse is a document as the API speaks it.
type documentResponse struct {
	UID            string     `json:"uid"`
	Kind           string     `json:"kind"`
	ElementType    string     `json:"element_type"`
	Site           int64      `json:"site"`
	CheckedOutBy   string     `json:"checked_out_by"`
	Workflow       string     `json:"workflow"`
	State          string     `json:"state"`
	CheckedOutName string     `json:"checked_out_by_name"`
	LockExpiresAt  *time.Time `json:"lock_expires_at"`
	AssignedTo     string     `json:"assigned_to"`
	AssignedToName string     `json:"assigned_to_name"`
	DueAt          *time.Time `json:"due_at"`
	Overdue        bool       `json:"overdue"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Version        *struct {
		Version     int        `json:"version"`
		Title       string     `json:"title"`
		Slug        string     `json:"slug"`
		CoverDate   string     `json:"cover_date"`
		Content     string     `json:"content"`
		Note        string     `json:"note"`
		Draft       bool       `json:"draft"`
		CheckedInAt *time.Time `json:"checked_in_at"`
		CreatedAt   time.Time  `json:"created_at"`
	} `json:"version"`
}

func newDocCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doc",
		Short: "Create, edit, and inspect documents",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newDocCreateCmd(),
		newDocShowCmd(),
		newDocListCmd(),
		newDocCheckoutCmd(),
		newDocCancelCmd(),
		newDocEditCmd(),
		newDocCheckinCmd(),
		newDocRevertCmd(),
		newDocDiffCmd(),
		newDocEventsCmd(),
		newDocTransitionsCmd(),
		newDocDoCmd(),
		newDocAssignCmd(),
		newDocDueCmd(),
		newDocCategoriesCmd(),
		newDocURIsCmd(),
		newDocPreviewCmd(),
		newDocPublishCmd(),
		newDocResourcesCmd(),
		newDocCommentCmd(),
		newDocCommentsCmd(),
		newDocResolveCmd(),
		newDocApproveCmd(),
		newDocApprovalsCmd(),
	)
	return cmd
}

// docClient builds a client from the stored credentials, letting --server win
// when it was given. Every document command starts this way, which is why it
// is one function.
func docClient(cmd *cobra.Command, server string) (*Client, error) {
	creds, err := LoadCredentials()
	if err != nil {
		return nil, err
	}
	if !cmd.Flags().Changed("server") && creds.Server != "" {
		server = creds.Server
	}
	return NewClient(server, creds.Token)
}

// docPath is the base path of one document. The uid is escaped: it is a ULID
// today, and a client that assumes what a server's identifiers look like is a
// client that breaks when they change.
func docPath(uid string) string { return "/api/v1/documents/" + url.PathEscape(uid) }

// readContent resolves the --content and --content-file flags to a body.
//
// A file named "-" is standard input, which is what makes an agent able to
// pipe a document in without a temporary file. There is no $EDITOR here: an
// editor needs a terminal, and the point of this command is to work without
// one.
func readContent(cmd *cobra.Command, content, file string) (string, bool, error) {
	switch {
	case cmd.Flags().Changed("content") && cmd.Flags().Changed("content-file"):
		return "", false, fmt.Errorf("--content and --content-file are two ways to say the same thing; give one")
	case cmd.Flags().Changed("content"):
		return content, true, nil
	case cmd.Flags().Changed("content-file"):
		if file == "-" {
			b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			if err != nil {
				return "", false, fmt.Errorf("reading the content from stdin: %w", err)
			}
			return string(b), true, nil
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return "", false, fmt.Errorf("reading %s: %w", file, err)
		}
		return string(b), true, nil
	default:
		return "", false, nil
	}
}

func addContentFlags(cmd *cobra.Command, content, file *string) {
	cmd.Flags().StringVar(content, "content", "", "the JSON element tree")
	cmd.Flags().StringVar(file, "content-file", "", `read the JSON element tree from a file, or "-" for stdin`)
}

func newDocCreateCmd() *cobra.Command {
	var (
		server      string
		asJSON      bool
		site        int64
		kind        string
		elementType string
		title       string
		slug        string
		coverDate   string
		content     string
		contentFile string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a document and its first working draft",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			body, _, err := readContent(cmd, content, contentFile)
			if err != nil {
				return err
			}

			req := map[string]any{
				"site": site, "kind": kind, "element_type": elementType, "title": title,
			}
			if slug != "" {
				req["slug"] = slug
			}
			if coverDate != "" {
				req["cover_date"] = coverDate
			}
			if body != "" {
				req["content"] = body
			}

			var doc documentResponse
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/documents", req, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, "created")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().Int64Var(&site, "site", 1, "the site the document belongs to")
	cmd.Flags().StringVar(&kind, "kind", "story", "story, media, or template")
	cmd.Flags().StringVar(&elementType, "element-type", "story", "the element type's key name")
	cmd.Flags().StringVar(&title, "title", "", "the document's title")
	cmd.Flags().StringVar(&slug, "slug", "", "the document's slug")
	cmd.Flags().StringVar(&coverDate, "cover-date", "", "the cover date")
	addContentFlags(cmd, &content, &contentFile)
	_ = cmd.MarkFlagRequired("title")
	return cmd
}

func newDocShowCmd() *cobra.Command {
	var (
		server  string
		asJSON  bool
		version int
	)
	cmd := &cobra.Command{
		Use:   "show UID",
		Short: "Print a document and the version being looked at",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			path := docPath(args[0])
			if cmd.Flags().Changed("version") {
				path = fmt.Sprintf("%s/versions/%d", path, version)
			}
			var doc documentResponse
			if err := client.Get(cmd.Context(), path, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, "")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().IntVar(&version, "version", 0, "show one numbered version instead of the current one")
	return cmd
}

// newDocListCmd is the queue query at the command line (PLAN.md M5
// acceptance 1).
//
// "--state review --unassigned" is the question the system we learned from
// could not express at all, because "unassigned" was not something it could
// name. Here it is two flags and one indexed query.
func newDocListCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		f      docListFlags
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the documents you may read",
		Long: "List the documents you may read.\n\n" +
			"The filters combine: \"--state review --unassigned\" is everything waiting\n" +
			"on an editor that nobody has picked up. --assignee takes a user uid or\n" +
			"\"me\"; --overdue is measured against the server's clock, not yours.\n\n" +
			"\"earl queue\" runs the combinations somebody has already named.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			if f.unassigned && cmd.Flags().Changed("assignee") {
				return fmt.Errorf("a document is assigned to somebody or to nobody, not both")
			}
			var out struct {
				Documents []documentResponse `json:"documents"`
			}
			if err := client.Get(cmd.Context(), listQuery(cmd, f), &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			return printDocTable(w, out.Documents)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&f.state, "state", "", "only documents in this workflow state")
	cmd.Flags().StringVar(&f.assignee, "assignee", "", `only documents assigned to this user uid, or "me"`)
	cmd.Flags().Int64Var(&f.site, "site", 0, "only documents on this site")
	cmd.Flags().BoolVar(&f.unassigned, "unassigned", false, "only documents nobody has been given")
	cmd.Flags().BoolVar(&f.overdue, "overdue", false, "only documents past their due date")
	cmd.Flags().IntVar(&f.limit, "limit", 100, "how many documents to list")
	return cmd
}

// newDocCheckoutCmd takes the edit lease.
func newDocCheckoutCmd() *cobra.Command {
	return simpleDocCmd("checkout UID", "Take the edit lease and open a working draft",
		http.MethodPost, "/checkout", "checked out")
}

// newDocCancelCmd releases the lease and keeps the draft.
func newDocCancelCmd() *cobra.Command {
	cmd := simpleDocCmd("cancel UID", "Release the edit lease, keeping the draft",
		http.MethodDelete, "/checkout", "checkout cancelled")
	cmd.Long = "Release the edit lease, keeping the draft.\n\n" +
		"This is not \"revert\": the draft survives with every edit in it, and the\n" +
		"next checkout picks it up where it was left."
	return cmd
}

// simpleDocCmd builds the commands that are one call with no body.
func simpleDocCmd(use, short, method, suffix, verb string) *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var doc documentResponse
			if err := client.Do(cmd.Context(), method, docPath(args[0])+suffix, nil, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, verb)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newDocEditCmd() *cobra.Command {
	var (
		server      string
		asJSON      bool
		title       string
		slug        string
		coverDate   string
		content     string
		contentFile string
	)
	cmd := &cobra.Command{
		Use:   "edit UID",
		Short: "Change the open working draft",
		Long: "Change the open working draft.\n\n" +
			"The document must be checked out by you. Only the flags you give are\n" +
			"changed: an omitted flag leaves the field alone, and a flag given as an\n" +
			"empty string clears it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			body, haveContent, err := readContent(cmd, content, contentFile)
			if err != nil {
				return err
			}

			// Changed rather than a zero-value comparison: an omitted field
			// and a field cleared to the empty string are different requests.
			req := map[string]any{}
			if cmd.Flags().Changed("title") {
				req["title"] = title
			}
			if cmd.Flags().Changed("slug") {
				req["slug"] = slug
			}
			if cmd.Flags().Changed("cover-date") {
				req["cover_date"] = coverDate
			}
			if haveContent {
				req["content"] = body
			}
			if len(req) == 0 {
				return fmt.Errorf("nothing to change; give --title, --slug, --cover-date, --content, or --content-file")
			}

			var doc documentResponse
			if err := client.Do(cmd.Context(), http.MethodPatch, docPath(args[0]), req, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, "edited")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&title, "title", "", "the document's title")
	cmd.Flags().StringVar(&slug, "slug", "", "the document's slug")
	cmd.Flags().StringVar(&coverDate, "cover-date", "", "the cover date")
	addContentFlags(cmd, &content, &contentFile)
	return cmd
}

func newDocCheckinCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		note   string
	)
	cmd := &cobra.Command{
		Use:   "checkin UID",
		Short: "Close the working draft, making it an immutable version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var doc documentResponse
			if err := client.Do(cmd.Context(), http.MethodPost, docPath(args[0])+"/checkin",
				map[string]any{"note": note}, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, "checked in")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&note, "note", "", "the check-in message")
	return cmd
}

func newDocRevertCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "revert UID",
		Short: "Discard the open working draft",
		Long: "Discard the open working draft.\n\n" +
			"A document whose draft was its only version is deleted: there is no\n" +
			"version 0 to fall back to. Use \"earl doc cancel\" to put a document down\n" +
			"without throwing the work away.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var doc documentResponse
			if err := client.Do(cmd.Context(), http.MethodPost, docPath(args[0])+"/revert", nil, &doc); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if doc.UID == "" {
				// 204: the draft was the document's only version, so the
				// document went with it.
				if asJSON {
					return writeJSON(w, map[string]any{"uid": args[0], "deleted": true})
				}
				fmt.Fprintf(w, "reverted: %s had no checked-in version and was deleted\n", args[0])
				return nil
			}
			return printDoc(w, doc, asJSON, "reverted")
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// diffResponse is GET /api/v1/documents/{uid}/diff.
type diffResponse struct {
	From   int    `json:"from"`
	To     int    `json:"to"`
	Text   string `json:"text"`
	Fields []struct {
		Field   string `json:"field"`
		Changed bool   `json:"changed"`
		Ops     []struct {
			Op   string `json:"op"`
			Text string `json:"text"`
		} `json:"ops"`
	} `json:"fields"`
}

// newDocDiffCmd is PLAN.md M3 acceptance 7.
//
// It prints the server's rendered text verbatim rather than re-rendering the
// ops, so that the golden file records one answer rather than this command's
// opinion of one.
func newDocDiffCmd() *cobra.Command {
	var (
		server   string
		asJSON   bool
		from, to int
	)
	cmd := &cobra.Command{
		Use:   "diff UID",
		Short: "Show the word-level difference between two versions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out diffResponse
			if err := client.Get(cmd.Context(),
				fmt.Sprintf("%s/diff?from=%d&to=%d", docPath(args[0]), from, to), &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			_, err = io.WriteString(w, out.Text)
			return err
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().IntVar(&from, "from", 0, "the older version number")
	cmd.Flags().IntVar(&to, "to", 0, "the newer version number")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// newDocEventsCmd prints a document's history, which is a query rather than a
// log grep (DESIGN.md 10).
func newDocEventsCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "events UID",
		Short: "Print a document's history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out struct {
				Events []struct {
					Type       string         `json:"type"`
					Name       string         `json:"name"`
					Actor      string         `json:"actor"`
					ActorName  string         `json:"actor_name"`
					Payload    map[string]any `json:"payload"`
					OccurredAt time.Time      `json:"occurred_at"`
				} `json:"events"`
			}
			if err := client.Get(cmd.Context(),
				fmt.Sprintf("%s/events?limit=%d", docPath(args[0]), limit), &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "WHEN\tWHAT\tWHO")
			for _, e := range out.Events {
				who := e.ActorName
				if who == "" {
					who = "the system"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", e.OccurredAt.Format(time.RFC3339), e.Name, who)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().IntVar(&limit, "limit", 100, "how many events to print")
	return cmd
}

// printDoc renders a document for a person, or as JSON.
func printDoc(w io.Writer, doc documentResponse, asJSON bool, verb string) error {
	if asJSON {
		return writeJSON(w, doc)
	}
	if verb != "" {
		fmt.Fprintf(w, "%s: %s\n", verb, doc.UID)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "uid\t%s\n", doc.UID)
	fmt.Fprintf(tw, "kind\t%s\n", doc.Kind)
	fmt.Fprintf(tw, "element type\t%s\n", doc.ElementType)
	if doc.State != "" {
		fmt.Fprintf(tw, "state\t%s\n", doc.State)
	}
	if doc.CheckedOutBy != "" {
		fmt.Fprintf(tw, "checked out by\t%s\n", doc.CheckedOutName)
		if doc.LockExpiresAt != nil {
			fmt.Fprintf(tw, "lease expires\t%s\n", doc.LockExpiresAt.Format(time.RFC3339))
		}
	} else {
		fmt.Fprintf(tw, "checked out by\t(nobody)\n")
	}
	if doc.AssignedTo != "" {
		fmt.Fprintf(tw, "assigned to\t%s\n", doc.AssignedToName)
	} else {
		fmt.Fprintf(tw, "assigned to\t(nobody)\n")
	}
	if doc.DueAt != nil {
		due := doc.DueAt.Format(time.RFC3339)
		if doc.Overdue {
			due += " (overdue)"
		}
		fmt.Fprintf(tw, "due\t%s\n", due)
	}
	if v := doc.Version; v != nil {
		state := fmt.Sprintf("%d (checked in %s)", v.Version, formatCheckedIn(v.CheckedInAt))
		if v.Draft {
			state = fmt.Sprintf("%d (working draft)", v.Version)
		}
		fmt.Fprintf(tw, "version\t%s\n", state)
		fmt.Fprintf(tw, "title\t%s\n", v.Title)
		if v.Slug != "" {
			fmt.Fprintf(tw, "slug\t%s\n", v.Slug)
		}
		if v.CoverDate != "" {
			fmt.Fprintf(tw, "cover date\t%s\n", v.CoverDate)
		}
		if v.Note != "" {
			fmt.Fprintf(tw, "note\t%s\n", v.Note)
		}
		if content := strings.TrimSpace(v.Content); content != "" && content != "{}" {
			fmt.Fprintf(tw, "content\t%s\n", content)
		}
	}
	return tw.Flush()
}

func formatCheckedIn(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// transitionsResponse is GET /api/v1/documents/{uid}/transitions.
type transitionsResponse struct {
	State       string `json:"state"`
	Transitions []struct {
		To        string   `json:"to"`
		Name      string   `json:"name"`
		Privilege string   `json:"privilege"`
		Permitted bool     `json:"permitted"`
		Reason    string   `json:"reason"`
		Guard     string   `json:"guard"`
		NeedsNote bool     `json:"needs_note"`
		Guards    []string `json:"guards"`
		Effects   []string `json:"effects"`
	} `json:"transitions"`
}

// newDocTransitionsCmd is PLAN.md M4 acceptance 1: every transition out of the
// current state, including the refused ones with a reason.
//
// Printing the refusals is the point. An action that silently does not exist
// teaches nobody anything and sends an editor to an administrator; "Approve --
// needs one more approval" tells them what to do next.
func newDocTransitionsCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "transitions UID",
		Short: "List the transitions out of a document's current state",
		Long: "List the transitions out of a document's current state.\n\n" +
			"Refused transitions are listed too, with the reason and the guard that\n" +
			"refused. A transition marked \"note\" needs one: pass it with\n" +
			"\"earl doc do UID --to STATE --note ...\".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out transitionsResponse
			if err := client.Get(cmd.Context(), docPath(args[0])+"/transitions", &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "state: %s\n", out.State)
			if len(out.Transitions) == 0 {
				fmt.Fprintln(w, "no transitions out of this state")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TO\tNAME\tNEEDS\tALLOWED\tWHY NOT")
			for _, t := range out.Transitions {
				needs := t.Privilege
				if t.NeedsNote {
					needs += ", note"
				}
				allowed := "yes"
				if !t.Permitted {
					allowed = "no"
				}
				why := t.Reason
				if why != "" && t.Guard != "" {
					why = fmt.Sprintf("%s (%s)", why, t.Guard)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.To, t.Name, needs, allowed, why)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newDocDoCmd performs one transition.
//
// It is "do" rather than "move" or "set-state" because the API models a
// transition as a subresource rather than as a state to assign: there is no
// way to say "put this document in published", only "make this declared move".
func newDocDoCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		to     string
		note   string
	)
	cmd := &cobra.Command{
		Use:   "do UID --to STATE",
		Short: "Perform one workflow transition",
		Long: "Perform one workflow transition.\n\n" +
			"The move must be one the workflow declares out of the document's current\n" +
			"state; anything else is refused, whoever is asking. Run\n" +
			"\"earl doc transitions UID\" to see what is available and why.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			req := map[string]any{"to": to}
			if note != "" {
				req["note"] = note
			}
			var doc documentResponse
			if err := client.Do(cmd.Context(), http.MethodPost,
				docPath(args[0])+"/transitions", req, &doc); err != nil {
				return err
			}
			return printDoc(cmd.OutOrStdout(), doc, asJSON, "moved to "+doc.State)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&to, "to", "", "the state to move to")
	cmd.Flags().StringVar(&note, "note", "", "the note the transition carries, when one is required")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// newDocCategoriesCmd shows or replaces where a document is filed
// (PLAN.md M7).
//
// With no --set it reads; with one or more it replaces, and the first is the
// primary category -- the one the URI is built from. Replacing rather than
// adding is what the route does, and it is what a person means: "this story
// belongs in features and in film" is one statement about the document, not a
// sequence of additions that could half succeed.
//
// It needs no checkout. A filing points at the document rather than at a
// version, so there is no draft copy of it to protect, and requiring a
// checkout to refile a story would make moving a section impossible while
// anybody was writing in it.
func newDocCategoriesCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		set    []string
	)
	cmd := &cobra.Command{
		Use:   "categories UID",
		Short: "Show or replace the categories a document is filed in",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			path := docPath(args[0]) + "/categories"

			var out struct {
				UID        string `json:"uid"`
				Categories []struct {
					Category string `json:"category"`
					Name     string `json:"name"`
					Primary  bool   `json:"primary"`
				} `json:"categories"`
			}
			if cmd.Flags().Changed("set") {
				err = client.Do(cmd.Context(), http.MethodPut, path,
					map[string]any{"categories": set}, &out)
			} else {
				err = client.Get(cmd.Context(), path, &out)
			}
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			if len(out.Categories) == 0 {
				fmt.Fprintln(w, "filed nowhere; a document with no category has no address")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CATEGORY\tNAME\tPRIMARY")
			for _, c := range out.Categories {
				fmt.Fprintf(tw, "%s\t%s\t%t\n", c.Category, c.Name, c.Primary)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringSliceVar(&set, "set", nil,
		"replace the filing with these category paths; the first is the primary one")
	return cmd
}

// newDocURIsCmd shows what address a document has in every output channel of
// its site (PLAN.md M7).
//
// This is the visible face of domain.BuildURI, and it is the command M7 exists
// for: a URI format is configuration somebody types and gets wrong, and the
// only alternative to showing them what it produces is publishing something to
// find out.
//
// A channel that cannot build one -- a format carrying a date against a
// version with no cover date -- reports its reason on its own line rather than
// failing the command, because the other channels still have answers.
func newDocURIsCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "uris UID",
		Short: "Show a document's address in every output channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out struct {
				UID  string `json:"uid"`
				URIs []struct {
					Channel string `json:"channel"`
					Name    string `json:"name"`
					URI     string `json:"uri"`
					File    string `json:"file"`
					URL     string `json:"url"`
					Error   string `json:"error"`
				} `json:"uris"`
			}
			if err := client.Get(cmd.Context(), docPath(args[0])+"/uris", &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CHANNEL\tURI\tFILE\tURL")
			for _, u := range out.URIs {
				if u.Error != "" {
					fmt.Fprintf(tw, "%s\t(no address)\t\t%s\n", u.Name, u.Error)
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Name, u.URI, u.File, u.URL)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// previewResponse is a rendered preview as the API speaks it.
type previewResponse struct {
	UID      string   `json:"uid"`
	Mode     string   `json:"mode"`
	Channel  string   `json:"channel"`
	Name     string   `json:"channel_name"`
	Version  int      `json:"version"`
	Draft    bool     `json:"draft"`
	Category string   `json:"category"`
	URI      string   `json:"uri"`
	URL      string   `json:"url"`
	Template string   `json:"template"`
	Searched []string `json:"searched"`
	Path     string   `json:"path"`
	Checksum string   `json:"checksum"`
	Bytes    int      `json:"bytes"`
	Valid    bool     `json:"valid"`
	Error    *struct {
		Template string `json:"template"`
		Line     int    `json:"line"`
		Phase    string `json:"phase"`
		Message  string `json:"message"`
	} `json:"error"`
}

// newDocPreviewCmd is "earl doc preview" (PLAN.md M8).
//
// It prints the rendered page by default, because that is what a person asking
// for a preview wants to see and because an agent driving this without a
// browser has no other way to look at one. --url prints the address instead,
// for somebody who does have a browser, and --validate asks the other question
// this route answers: whether the template compiles at all.
func newDocPreviewCmd() *cobra.Command {
	var (
		server   string
		asJSON   bool
		channel  string
		validate bool
		asURL    bool
	)
	cmd := &cobra.Command{
		Use:   "preview UID",
		Short: "Render a document and print it, its address, or its template's errors",
		Long: "Render a document and print it.\n\n" +
			"The rendered page is written to the server's preview tree and served\n" +
			"back under /preview/; --url prints that address instead of the page.\n" +
			"--validate parses the template and writes nothing, which is the way to\n" +
			"ask whether a template compiles without publishing something to find\n" +
			"out.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if validate && asURL {
				return fmt.Errorf("--validate writes no preview, so there is no --url to print")
			}
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}

			body := map[string]any{}
			if channel != "" {
				body["channel"] = channel
			}
			if validate {
				body["validate"] = true
			}
			var out previewResponse
			if err := client.Do(cmd.Context(), http.MethodPost, docPath(args[0])+"/preview", body, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				if err := writeJSON(w, out); err != nil {
					return err
				}
				if !out.Valid {
					return errTemplateInvalid
				}
				return nil
			}

			if validate {
				if out.Valid {
					fmt.Fprintf(w, "ok\t%s\n", out.Template)
					return nil
				}
				fmt.Fprintf(w, "invalid\t%s\n", out.Template)
				if out.Error != nil {
					fmt.Fprintf(w, "%s\n", out.Error.Message)
				}
				return errTemplateInvalid
			}

			if asURL {
				fmt.Fprintln(w, client.Server+out.Path)
				return nil
			}

			page, err := client.GetText(cmd.Context(), out.Path)
			if err != nil {
				return err
			}
			_, err = io.WriteString(w, page)
			return err
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&channel, "channel", "",
		"the output channel's uid; needed only when the site has more than one")
	cmd.Flags().BoolVar(&validate, "validate", false,
		"parse the template and report what is wrong with it, writing nothing")
	cmd.Flags().BoolVar(&asURL, "url", false,
		"print the preview's address instead of the rendered page")
	return cmd
}

// errTemplateInvalid is what "earl doc preview --validate" exits with when the
// template does not compile.
//
// The report has already been printed on stdout; this is what makes the exit
// code say so as well. An agent driving this reads exit codes, and "invalid"
// on stdout with a zero exit is a report nobody acts on.
var errTemplateInvalid = errors.New("the template does not compile")

// publicationResponse is a scheduled publish as the API speaks it.
type publicationResponse struct {
	UID          string    `json:"uid"`
	Version      int       `json:"version"`
	Job          string    `json:"job"`
	ScheduledFor time.Time `json:"scheduled_for"`
	Channels     []struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	} `json:"channels"`

	// Related and Refusals are M10's: what the cascade gathered beside the
	// root, and what it would not publish.
	Related []struct {
		UID     string `json:"uid"`
		Title   string `json:"title"`
		Version int    `json:"version"`
		Job     string `json:"job"`
	} `json:"related"`
	Refusals []publicationRefusal `json:"refusals"`

	DryRun      bool `json:"dry_run"`
	WouldRefuse bool `json:"would_refuse"`
}

// publicationRefusal is one document the cascade will not publish. It is the
// same shape on a 200, a 202, and the 409 a refused cascade produces, which is
// what lets one printer serve all three.
type publicationRefusal struct {
	UID          string `json:"uid"`
	Title        string `json:"title"`
	ReferencedBy string `json:"referenced_by"`
	Reason       string `json:"reason"`
	Detail       string `json:"detail"`
}

// newDocPublishCmd is "earl doc publish" (PLAN.md M9).
//
// It prints the version it pinned, because that is the whole promise being
// made: an editor who schedules a publish for midnight and keeps working needs
// to see that it is version 5 that will appear, whatever the draft becomes
// (invariant 8). A command that printed only "scheduled" would leave the one
// fact worth knowing invisible.
//
// --at takes what a person types: a date, a timestamp, or a duration. "--at
// 2h" and "--at 2026-03-01T00:00:00Z" are the same grammar the due date takes,
// which is the sort of consistency somebody notices only when it is missing.
func newDocPublishCmd() *cobra.Command {
	var (
		server   string
		asJSON   bool
		at       string
		channels []string
		dryRun   bool
	)
	cmd := &cobra.Command{
		Use:   "publish UID",
		Short: "Schedule a publish of a document's newest checked-in version",
		Long: "Schedule a publish of a document's newest checked-in version.\n\n" +
			"The version is pinned now, when the request is made, and never resolved\n" +
			"at the scheduled hour: version 5 approved for midnight is what appears\n" +
			"at midnight, whatever the draft has become. Without --channel the\n" +
			"document is published to every output channel of its site.\n\n" +
			"Publishing a document publishes the documents it references, each with\n" +
			"its own pinned version. A referenced document that cannot be published\n" +
			"-- you may not, its workflow does not call its state publishable,\n" +
			"somebody has it checked out, or it has never been checked in -- is\n" +
			"refused by name. Whether that stops the whole publish is the server's\n" +
			"publish.related_failure setting. Use --dry-run to see the set and the\n" +
			"refusals without scheduling anything.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}

			body := map[string]any{}
			if at != "" {
				body["at"] = at
			}
			if len(channels) > 0 {
				body["channels"] = channels
			}
			if dryRun {
				body["dry_run"] = true
			}
			var out publicationResponse
			if err := client.Do(cmd.Context(), http.MethodPost, docPath(args[0])+"/publications", body, &out); err != nil {
				// A cascade refused under publish.related_failure = fail is a
				// 409 whose problem document names every document that held
				// it up. The message already spells them out; the table is
				// printed as well because a list of five is a list somebody
				// reads down a column rather than out of a sentence.
				var problem *Problem
				if errors.As(err, &problem) && len(problem.Refusals) > 0 {
					printRefusals(cmd.ErrOrStderr(), problem.Refusals)
				}
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}

			if out.DryRun {
				fmt.Fprintln(w, "dry run: nothing was scheduled")
			}
			if !out.WouldRefuse {
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "DOCUMENT\tVERSION\tJOB\tSCHEDULED FOR")
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
					out.UID, out.Version, out.Job, out.ScheduledFor.UTC().Format(time.RFC3339))
				if err := tw.Flush(); err != nil {
					return err
				}
			}

			// The related documents get a table of their own rather than more
			// rows in the one above. They are a different thing -- documents
			// dragged along by the one that was asked for -- and a row that
			// had to say so in a column meant for a timestamp would be a
			// table explaining itself.
			if len(out.Related) > 0 {
				fmt.Fprintf(w, "\n%d referenced document(s) publish with it:\n", len(out.Related))
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "DOCUMENT\tTITLE\tVERSION\tJOB")
				for _, rel := range out.Related {
					fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", rel.UID, rel.Title, rel.Version, rel.Job)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}

			if len(out.Refusals) > 0 {
				printRefusals(w, out.Refusals)
			}
			if out.WouldRefuse {
				// The dry run of a publish this server would refuse. Said
				// plainly, because there is no table above it and the absence
				// of one is not an explanation.
				fmt.Fprintln(w, "\nthis server's publish.related_failure is \"fail\", so nothing would be published")
			}
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&at, "at", "",
		"when to publish: a date, a timestamp, or a duration from now; the default is now")
	cmd.Flags().StringSliceVar(&channels, "channel", nil,
		"an output channel's uid; repeatable, and the default is every channel of the site")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report the documents this publish would gather and the ones it would refuse, and schedule nothing")
	return cmd
}

// printRefusals writes the documents a cascade would not publish
// (PLAN.md M10 acceptance 2, 4, 5).
//
// Each is named, with the document that referenced it and the reason, because
// a refusal an editor cannot act on is a refusal that gets ignored: "3 related
// documents could not be published" sends somebody looking through a story one
// reference at a time.
func printRefusals(w io.Writer, refusals []publicationRefusal) {
	fmt.Fprintf(w, "\n%d referenced document(s) will not be published:\n", len(refusals))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DOCUMENT\tTITLE\tREFERENCED BY\tREASON\tWHY")
	for _, r := range refusals {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.UID, r.Title, r.ReferencedBy, r.Reason, r.Detail)
	}
	_ = tw.Flush()
}

// resourcesResponse is what the publisher has written for a document.
type resourcesResponse struct {
	UID       string `json:"uid"`
	Live      int64  `json:"live_version_id"`
	Resources []struct {
		Channel     string    `json:"channel_name"`
		URI         string    `json:"uri"`
		Path        string    `json:"path"`
		Version     int64     `json:"version_id"`
		Checksum    string    `json:"checksum"`
		Bytes       int64     `json:"bytes"`
		PublishedAt time.Time `json:"published_at"`
	} `json:"resources"`
}

// newDocResourcesCmd is "earl doc resources" (PLAN.md M9).
//
// It is how somebody sees that a slug change took the old file with it. The
// alternative is looking at the output tree by hand, which is exactly what a
// system that remembers what it wrote exists to make unnecessary.
func newDocResourcesCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "resources UID",
		Short: "Show the files the publisher has written for a document",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out resourcesResponse
			if err := client.Get(cmd.Context(), docPath(args[0])+"/resources", &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CHANNEL\tURI\tPATH\tBYTES\tPUBLISHED")
			for _, r := range out.Resources {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
					r.Channel, r.URI, r.Path, r.Bytes, r.PublishedAt.UTC().Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}
