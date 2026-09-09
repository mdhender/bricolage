// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The comment and approval commands (PLAN.md M11).
//
// earl is the acceptance-test harness for every milestone, not a debug toy: if
// earl cannot do it, the API is incomplete. Everything M11 adds to the API is
// reachable from here -- opening a thread, replying to one, resolving one,
// signing off, and taking the sign-off back.

// commentResponse is one comment as the API speaks it.
type commentResponse struct {
	UID            string     `json:"uid"`
	Author         string     `json:"author"`
	AuthorName     string     `json:"author_name"`
	Body           string     `json:"body"`
	Version        int        `json:"version"`
	Resolved       bool       `json:"resolved"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	ResolvedBy     string     `json:"resolved_by"`
	ResolvedByName string     `json:"resolved_by_name"`
	CreatedAt      time.Time  `json:"created_at"`

	Replies []commentResponse `json:"replies"`
}

type commentsResponse struct {
	UID     string            `json:"uid"`
	Open    int               `json:"open"`
	Threads []commentResponse `json:"threads"`
}

// approvalsResponse is what every approval route answers with.
type approvalsResponse struct {
	UID       string `json:"uid"`
	State     string `json:"state"`
	Version   int    `json:"version"`
	Current   bool   `json:"current"`
	Count     int    `json:"count"`
	Required  int    `json:"required"`
	Met       bool   `json:"met"`
	Approved  bool   `json:"approved"`
	Withdrawn bool   `json:"withdrawn"`

	Approvals []struct {
		User     string    `json:"user"`
		UserName string    `json:"user_name"`
		State    string    `json:"state"`
		At       time.Time `json:"at"`
	} `json:"approvals"`
}

// newDocCommentsCmd lists the discussion on a document.
func newDocCommentsCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "comments UID",
		Short: "List the comment threads on a document",
		Long: "List the comment threads on a document.\n\n" +
			"An open thread refuses any transition guarded by comments_resolved.\n" +
			"Close one with \"earl doc resolve COMMENT\", which takes the uid printed\n" +
			"in the first column.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out commentsResponse
			if err := client.Get(cmd.Context(), docPath(args[0])+"/comments", &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			if len(out.Threads) == 0 {
				fmt.Fprintln(w, "no comments")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UID\tWHO\tWHEN\tSTATE\tCOMMENT")
			for _, t := range out.Threads {
				printComment(tw, t, "")
				for _, r := range t.Replies {
					printComment(tw, r, "  ")
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(w, "\n%s open\n", plural(out.Open, "thread"))
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// printComment writes one line of the discussion. A reply is indented and
// carries no state of its own: a thread is resolved as a whole.
func printComment(w *tabwriter.Writer, c commentResponse, indent string) {
	state := "open"
	switch {
	case indent != "":
		state = "-"
	case c.Resolved:
		state = "resolved by " + displayName(c.ResolvedByName, c.ResolvedBy)
	}
	fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\n",
		indent, c.UID, displayName(c.AuthorName, c.Author),
		c.CreatedAt.UTC().Format(time.RFC3339), state, oneLine(c.Body))
}

// newDocCommentCmd opens a thread, or replies to one.
func newDocCommentCmd() *cobra.Command {
	var (
		server  string
		asJSON  bool
		replyTo string
	)
	cmd := &cobra.Command{
		Use:   "comment UID BODY",
		Short: "Comment on a document, or reply to a thread",
		Long: "Comment on a document, or reply to a thread.\n\n" +
			"Commenting needs only read access: raising a concern is what a\n" +
			"fact-checker or a lawyer does about a story they may see and may not\n" +
			"touch. It needs no checkout, and the comment records the version being\n" +
			"looked at.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			req := map[string]any{"body": args[1]}
			if replyTo != "" {
				req["reply_to"] = replyTo
			}
			var out commentResponse
			if err := client.Do(cmd.Context(), http.MethodPost,
				docPath(args[0])+"/comments", req, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			what := "thread opened"
			if replyTo != "" {
				what = "replied"
			}
			fmt.Fprintf(w, "%s: %s\n", what, out.UID)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&replyTo, "reply-to", "", "the uid of the thread to reply to")
	return cmd
}

// newDocResolveCmd closes a thread.
//
// It takes a comment's uid rather than a document's, which is why the route is
// POST /api/v1/comments/{uid}/resolution: deciding a question is settled is an
// act on the discussion rather than on the document it hangs from.
func newDocResolveCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "resolve COMMENT",
		Short: "Resolve a comment thread",
		Long: "Resolve a comment thread.\n\n" +
			"The argument is a comment's uid, from \"earl doc comments UID\". A reply\n" +
			"cannot be resolved on its own: a thread is settled as a whole.\n\n" +
			"Resolving needs edit access over the document, or being the person who\n" +
			"opened the thread.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out commentResponse
			if err := client.Do(cmd.Context(), http.MethodPost,
				"/api/v1/comments/"+url.PathEscape(args[0])+"/resolution", nil, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "resolved by %s\n", displayName(out.ResolvedByName, out.ResolvedBy))
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newDocApproveCmd records the caller's sign-off, or takes it back.
func newDocApproveCmd() *cobra.Command {
	var (
		server   string
		asJSON   bool
		withdraw bool
	)
	cmd := &cobra.Command{
		Use:   "approve UID",
		Short: "Approve the current version of a document",
		Long: "Approve the current version of a document, in the state it is in.\n\n" +
			"Approving twice is not an error and not two approvals. --withdraw takes\n" +
			"your own approval back, and is equally forgiving about one that is not\n" +
			"there.\n\n" +
			"An approval attaches to a checked-in version, so a document whose\n" +
			"current version is still an open working draft is refused: sign-off on\n" +
			"something that is still being written is sign-off that a check-in would\n" +
			"not invalidate.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out approvalsResponse
			if withdraw {
				err = client.Do(cmd.Context(), http.MethodDelete,
					docPath(args[0])+"/approvals/current", nil, &out)
			} else {
				err = client.Do(cmd.Context(), http.MethodPost,
					docPath(args[0])+"/approvals", nil, &out)
			}
			if err != nil {
				return err
			}
			return printApprovals(cmd.OutOrStdout(), out, asJSON)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().BoolVar(&withdraw, "withdraw", false, "remove your own approval instead of recording one")
	return cmd
}

// newDocApprovalsCmd lists who has signed off.
func newDocApprovalsCmd() *cobra.Command {
	var (
		server  string
		asJSON  bool
		version int
	)
	cmd := &cobra.Command{
		Use:   "approvals UID",
		Short: "Show who has approved a document, and how many the process wants",
		Long: "Show who has approved a document, and how many the process wants.\n\n" +
			"Approvals attach to a version, so checking in a new one drops the count\n" +
			"to zero; --version N shows an older version's, which stay exactly as\n" +
			"they were.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			path := docPath(args[0]) + "/approvals"
			if version > 0 {
				path += "?version=" + strconv.Itoa(version)
			}
			var out approvalsResponse
			if err := client.Get(cmd.Context(), path, &out); err != nil {
				return err
			}
			return printApprovals(cmd.OutOrStdout(), out, asJSON)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().IntVar(&version, "version", 0, "show the approvals of this version instead of the current one")
	return cmd
}

func printApprovals(w io.Writer, out approvalsResponse, asJSON bool) error {
	if asJSON {
		return writeJSON(w, out)
	}
	// Three sentences, because the question has three shapes. An older
	// version has no live one to answer and reports what it collected; a
	// state the process asks nothing of would otherwise read "0 of 0", which
	// looks like a requirement nobody has met rather than no requirement.
	switch {
	case !out.Current:
		fmt.Fprintf(w, "version %d: %s recorded\n", out.Version, plural(out.Count, "approval"))
	case out.Required == 0:
		fmt.Fprintf(w, "version %d in %s: %s, and none are required to leave this state\n",
			out.Version, out.State, plural(out.Count, "approval"))
	default:
		fmt.Fprintf(w, "version %d in %s: %d of %d\n", out.Version, out.State, out.Count, out.Required)
	}
	if len(out.Approvals) == 0 {
		fmt.Fprintln(w, "nobody has approved this version")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHO\tSTATE\tWHEN")
	for _, a := range out.Approvals {
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			displayName(a.UserName, a.User), a.State, a.At.UTC().Format(time.RFC3339))
	}
	return tw.Flush()
}

// displayName prefers the name and falls back to the uid, so that a line about
// a user who has since been removed still names somebody.
func displayName(name, uid string) string {
	if name != "" {
		return name
	}
	if uid != "" {
		return uid
	}
	return "-"
}

// oneLine folds a comment body onto one line for a table. The full text is in
// --json, which is where a client that wants it should be looking.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const limit = 60
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
