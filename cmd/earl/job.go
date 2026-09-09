// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The job commands (DESIGN.md 11, PLAN.md M6).
//
// earl is the acceptance-test harness for every milestone, so everything M6
// adds to the API is reachable from here: the queue as it stands, the failures
// on their own, and the one button that puts a failure back.
//
// There is no "earl job create", and there will not be one. Work is scheduled
// by the operation that needs it -- "earl publish", in M9 -- and a command
// that posted an arbitrary kind and payload would be a way to run any handler
// in the server with arguments the operator typed.

// jobResponse is GET /api/v1/jobs and POST /api/v1/jobs/{uid}/retry.
type jobResponse struct {
	UID            string     `json:"uid"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Priority       int        `json:"priority"`
	ScheduledFor   time.Time  `json:"scheduled_for"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	LastError      string     `json:"last_error"`
	Worker         string     `json:"worker"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	CompletedAt    *time.Time `json:"completed_at"`
	FailedAt       *time.Time `json:"failed_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// newJobCmd is "earl job list" and "earl job retry".
func newJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Look at the background job queue, and retry what failed",
		Long: "Look at the background job queue, and retry what failed.\n\n" +
			"Jobs are scheduled by the operations that need them; there is no way\n" +
			"to create one from here, deliberately.",
	}
	cmd.AddCommand(newJobListCmd(), newJobRetryCmd())
	return cmd
}

// newJobListCmd is "earl job list [--failed|--pending]" (DESIGN.md 11).
//
// The two flags are the two questions worth asking: what is waiting, and what
// did I lose. They are mutually exclusive because a job is one or the other,
// and asking for both would be asking for a list of nothing.
func newJobListCmd() *cobra.Command {
	var (
		server  string
		asJSON  bool
		pending bool
		failed  bool
		kind    string
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List background jobs",
		Long: "List background jobs.\n\n" +
			"--pending lists what has not finished, in the order the workers will\n" +
			"take it: priority first, then schedule. --failed lists what ran out of\n" +
			"attempts, newest failure first. With neither, the newest jobs come\n" +
			"first whatever became of them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pending && failed {
				return fmt.Errorf("a job is pending or failed, not both; give one of --pending and --failed")
			}
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}

			q := url.Values{}
			q.Set("limit", strconv.Itoa(limit))
			if pending {
				q.Set("pending", "true")
			}
			if failed {
				q.Set("failed", "true")
			}
			if cmd.Flags().Changed("kind") {
				q.Set("kind", kind)
			}

			var out struct {
				Jobs  []jobResponse `json:"jobs"`
				Count int           `json:"count"`
				Asks  string        `json:"asks"`
			}
			if err := client.Get(cmd.Context(), "/api/v1/jobs?"+q.Encode(), &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			if len(out.Jobs) == 0 {
				// The question is printed with the empty answer, so that "no
				// jobs" says which list was asked for rather than looking like
				// a broken queue.
				fmt.Fprintf(w, "no %s\n", out.Asks)
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UID\tKIND\tSTATUS\tPRI\tATTEMPTS\tSCHEDULED\tWORKER\tLAST ERROR")
			for _, j := range out.Jobs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d/%d\t%s\t%s\t%s\n",
					j.UID, j.Kind, j.Status, j.Priority,
					j.Attempts, j.MaxAttempts,
					j.ScheduledFor.Format(time.RFC3339),
					dash(j.Worker), dash(truncate(j.LastError, 60)))
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().BoolVar(&pending, "pending", false, "list only the jobs that have not finished")
	cmd.Flags().BoolVar(&failed, "failed", false, "list only the jobs that ran out of attempts")
	cmd.Flags().StringVar(&kind, "kind", "", "list only jobs of this kind")
	cmd.Flags().IntVar(&limit, "limit", 100, "how many jobs to list")
	return cmd
}

// newJobRetryCmd is "earl job retry ID" (DESIGN.md 11).
//
// The ID is the job's uid, which is what the API speaks (invariant 10) and
// what "earl job list" prints in its first column.
func newJobRetryCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "retry UID",
		Short: "Put a failed job back on the queue",
		Long: "Put a failed job back on the queue.\n\n" +
			"The attempt count goes back to zero: a retry is a decision to try the\n" +
			"whole thing again, not to squeeze one more attempt out of an exhausted\n" +
			"budget. A job that has not failed is refused rather than reset.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var job jobResponse
			if err := client.Do(cmd.Context(), http.MethodPost,
				"/api/v1/jobs/"+url.PathEscape(args[0])+"/retry", nil, &job); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, job)
			}
			fmt.Fprintf(w, "%s (%s) retried: %s, scheduled for %s\n",
				job.UID, job.Kind, job.Status, job.ScheduledFor.Format(time.RFC3339))
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// dash renders an empty column as "-" rather than as nothing, so that a table
// with a missing value still has the right number of columns to read.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncate bounds a message to a column width. A job's last error is whatever
// a handler returned, and one of them will eventually be a stack trace.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
