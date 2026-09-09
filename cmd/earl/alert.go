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

// The alert-rule and notification commands (PLAN.md M12).
//
// earl is the acceptance-test harness for every milestone, not a debug toy: if
// earl cannot do it, the API is incomplete. Everything M12 adds is reachable
// from here -- writing a rule, turning one off, deleting one, reading an
// inbox, and marking a line in it read -- and so is the thing a person most
// needs, which is the list of event types a rule may watch. A rule engine
// whose vocabulary you have to read the source to discover is a rule engine
// nobody configures correctly.

// conditionRequest is one condition as the API speaks it. Value is "any"
// because "in" carries a list and everything else carries a scalar.
type conditionRequest struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// alertRuleResponse is a rule as the API speaks it.
type alertRuleResponse struct {
	UID           string             `json:"uid"`
	Name          string             `json:"name"`
	EventType     string             `json:"event_type"`
	EventName     string             `json:"event_name"`
	Conditions    []conditionRequest `json:"conditions"`
	Channel       string             `json:"channel"`
	Target        string             `json:"target"`
	Active        bool               `json:"active"`
	CreatedBy     string             `json:"created_by"`
	CreatedByName string             `json:"created_by_name"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

type alertRulesResponse struct {
	Rules      []alertRuleResponse `json:"rules"`
	EventTypes []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"event_types"`
}

// notificationResponse is one line of an inbox.
type notificationResponse struct {
	UID         string         `json:"uid"`
	Read        bool           `json:"read"`
	ReadAt      *time.Time     `json:"read_at"`
	CreatedAt   time.Time      `json:"created_at"`
	Rule        string         `json:"rule"`
	RuleName    string         `json:"rule_name"`
	EventType   string         `json:"event_type"`
	EventName   string         `json:"event_name"`
	SubjectKind string         `json:"subject_kind"`
	Payload     map[string]any `json:"payload"`
	OccurredAt  time.Time      `json:"occurred_at"`
}

type notificationsResponse struct {
	Unread        int                    `json:"unread"`
	Notifications []notificationResponse `json:"notifications"`
}

const alertPath = "/api/v1/alert-rules"

// newAlertCmd is the alert rule group.
func newAlertCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alert",
		Short: "Manage the alert rules that turn events into notifications",
		Long: "Manage the alert rules that turn events into notifications.\n\n" +
			"A rule watches one event type and fires when every one of its conditions\n" +
			"passes. Conditions are an AND; \"or\" is two rules. Fields resolve from\n" +
			"the acting user, then the event's payload, then its subject, in that\n" +
			"order.\n\n" +
			"Writing a rule needs \"create\" over the system, which is what an element\n" +
			"type needs and for the same reason: it is configuration the whole\n" +
			"installation shares.",
	}
	cmd.AddCommand(newAlertListCmd(), newAlertShowCmd(), newAlertCreateCmd(),
		newAlertUpdateCmd(), newAlertDeleteCmd(), newAlertEventsCmd())
	return cmd
}

func newAlertListCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the alert rules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out alertRulesResponse
			if err := client.Get(cmd.Context(), alertPath, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			if len(out.Rules) == 0 {
				fmt.Fprintln(w, "no alert rules")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UID\tNAME\tEVENT\tCHANNEL\tTARGET\tSTATE\tCONDITIONS")
			for _, r := range out.Rules {
				state := "active"
				if !r.Active {
					state = "off"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.UID, r.Name, r.EventType, r.Channel, r.Target, state, conditionSummary(r.Conditions))
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newAlertShowCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "show UID",
		Short: "Show one alert rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out alertRuleResponse
			if err := client.Get(cmd.Context(), alertPath+"/"+url.PathEscape(args[0]), &out); err != nil {
				return err
			}
			return printAlertRule(cmd.OutOrStdout(), out, asJSON)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newAlertCreateCmd() *cobra.Command {
	var (
		server     string
		asJSON     bool
		name       string
		eventType  string
		channel    string
		target     string
		conditions []string
		inactive   bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Write an alert rule",
		Long: "Write an alert rule.\n\n" +
			"--condition takes FIELD:OP:VALUE and may be given more than once; all of\n" +
			"them must pass. The operators are eq, ne, lt, lte, gt, gte, in, contains,\n" +
			"and matches. \"in\" takes a comma-separated list. \"matches\" takes a Go\n" +
			"regexp, and one that does not compile is refused here rather than at fire\n" +
			"time, where nobody would see it.\n\n" +
			"--target is user:UID, role:SLUG, or -- on the email channel only --\n" +
			"email:ADDRESS.\n\n" +
			"Example:\n" +
			"  earl alert create --name \"Legal desk\" --event document.transitioned \\\n" +
			"      --channel in_app --target role:legal --condition to:eq:legal",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			parsed, err := parseConditions(conditions)
			if err != nil {
				return err
			}
			req := map[string]any{
				"name":       name,
				"event_type": eventType,
				"channel":    channel,
				"target":     target,
				"conditions": parsed,
				"active":     !inactive,
			}
			var out alertRuleResponse
			if err := client.Do(cmd.Context(), http.MethodPost, alertPath, req, &out); err != nil {
				return err
			}
			return printAlertRule(cmd.OutOrStdout(), out, asJSON)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&name, "name", "", "what to call the rule")
	cmd.Flags().StringVar(&eventType, "event", "", "the event type to watch, from \"earl alert events\"")
	cmd.Flags().StringVar(&channel, "channel", "in_app", "in_app or email")
	cmd.Flags().StringVar(&target, "target", "", "user:UID, role:SLUG, or email:ADDRESS")
	cmd.Flags().StringArrayVar(&conditions, "condition", nil, "FIELD:OP:VALUE; repeatable, and all of them must pass")
	cmd.Flags().BoolVar(&inactive, "inactive", false, "write the rule switched off")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("event")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}

func newAlertUpdateCmd() *cobra.Command {
	var (
		server     string
		asJSON     bool
		name       string
		eventType  string
		channel    string
		target     string
		conditions []string
		active     bool
	)
	cmd := &cobra.Command{
		Use:   "update UID",
		Short: "Change an alert rule, or turn one off",
		Long: "Change an alert rule, or turn one off.\n\n" +
			"Only the flags you give are changed. --active=false switches a rule off\n" +
			"without deleting it, which is what you want for the duration of a bulk\n" +
			"import: the conditions are still there on Friday.\n\n" +
			"--condition replaces the whole list, because a rule is an AND of its\n" +
			"conditions and there is no way to name one of them for removal.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			req := map[string]any{}
			if cmd.Flags().Changed("name") {
				req["name"] = name
			}
			if cmd.Flags().Changed("event") {
				req["event_type"] = eventType
			}
			if cmd.Flags().Changed("channel") {
				req["channel"] = channel
			}
			if cmd.Flags().Changed("target") {
				req["target"] = target
			}
			if cmd.Flags().Changed("condition") {
				parsed, err := parseConditions(conditions)
				if err != nil {
					return err
				}
				req["conditions"] = parsed
			}
			if cmd.Flags().Changed("active") {
				req["active"] = active
			}
			if len(req) == 0 {
				return fmt.Errorf("nothing to change; give at least one flag")
			}
			var out alertRuleResponse
			if err := client.Do(cmd.Context(), http.MethodPatch,
				alertPath+"/"+url.PathEscape(args[0]), req, &out); err != nil {
				return err
			}
			return printAlertRule(cmd.OutOrStdout(), out, asJSON)
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&name, "name", "", "what to call the rule")
	cmd.Flags().StringVar(&eventType, "event", "", "the event type to watch")
	cmd.Flags().StringVar(&channel, "channel", "", "in_app or email")
	cmd.Flags().StringVar(&target, "target", "", "user:UID, role:SLUG, or email:ADDRESS")
	cmd.Flags().StringArrayVar(&conditions, "condition", nil, "FIELD:OP:VALUE; replaces the whole list")
	cmd.Flags().BoolVar(&active, "active", true, "switch the rule on or off")
	return cmd
}

func newAlertDeleteCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "delete UID",
		Short: "Delete an alert rule",
		Long: "Delete an alert rule.\n\n" +
			"The notifications it already produced survive it: what a rule told\n" +
			"somebody still happened. To stop a rule firing without losing its\n" +
			"conditions, use \"earl alert update UID --active=false\".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			if err := client.Do(cmd.Context(), http.MethodDelete,
				alertPath+"/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
	addServerFlag(cmd, &server)
	return cmd
}

// newAlertEventsCmd prints the vocabulary a rule may watch.
//
// It reads the list off the server rather than carrying a copy, because a
// client with its own copy of the registry is the seeded 153-row table the
// design replaced, coming back in another form.
func newAlertEventsCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "List the event types a rule may watch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out alertRulesResponse
			if err := client.Get(cmd.Context(), alertPath, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out.EventTypes)
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TYPE\tNAME")
			for _, e := range out.EventTypes {
				fmt.Fprintf(tw, "%s\t%s\n", e.Type, e.Name)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newNotificationCmd is the inbox group.
func newNotificationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "notification",
		Aliases: []string{"notifications"},
		Short:   "Read your own notifications",
		Long: "Read your own notifications.\n\n" +
			"An inbox is a person's. There is no way to read anybody else's and\n" +
			"deliberately no privilege that would open one.",
	}
	cmd.AddCommand(newNotificationListCmd(), newNotificationReadCmd())
	return cmd
}

func newNotificationListCmd() *cobra.Command {
	var (
		server string
		asJSON bool
		unread bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your notifications, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			path := "/api/v1/notifications"
			q := url.Values{}
			if unread {
				q.Set("unread", "true")
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var out notificationsResponse
			if err := client.Get(cmd.Context(), path, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			if len(out.Notifications) == 0 {
				fmt.Fprintln(w, "no notifications")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UID\tWHEN\tSTATE\tEVENT\tABOUT\tRULE")
			for _, n := range out.Notifications {
				state := "unread"
				if n.Read {
					state = "read"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					n.UID, n.OccurredAt.UTC().Format(time.RFC3339), state,
					n.EventName, notificationSubject(n), displayName(n.RuleName, n.Rule))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(w, "\n%d unread\n", out.Unread)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().BoolVar(&unread, "unread", false, "only the ones you have not read")
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many")
	return cmd
}

func newNotificationReadCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "read UID",
		Short: "Mark one of your notifications read",
		Long: "Mark one of your notifications read.\n\n" +
			"Reading one twice is not an error and not two reads.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out notificationResponse
			if err := client.Do(cmd.Context(), http.MethodPost,
				"/api/v1/notifications/"+url.PathEscape(args[0])+"/read", nil, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "read: %s\n", out.EventName)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// parseConditions turns the --condition flags into what the API wants.
//
// The value is everything after the second colon, so a value may contain one:
// a URI, a timestamp, and a regexp all do. "in" splits its value on commas,
// because a list is what that operator means and a person typing one types it
// with commas.
func parseConditions(raw []string) ([]conditionRequest, error) {
	out := make([]conditionRequest, 0, len(raw))
	for _, s := range raw {
		parts := strings.SplitN(s, ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("condition %q: want FIELD:OP:VALUE", s)
		}
		c := conditionRequest{Field: parts[0], Op: parts[1], Value: parts[2]}
		if parts[1] == "in" {
			c.Value = strings.Split(parts[2], ",")
		}
		out = append(out, c)
	}
	return out, nil
}

// conditionSummary renders a rule's conditions for one cell of a table. A rule
// with none matches every event of its type, which is a real configuration and
// says so rather than showing an empty cell.
func conditionSummary(cs []conditionRequest) string {
	if len(cs) == 0 {
		return "(every one)"
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s %s %v", c.Field, c.Op, c.Value))
	}
	return strings.Join(parts, " AND ")
}

// notificationSubject names what an event was about, from its payload. Every
// event type in this system writes a uid into its payload, and the subject
// kind beside it says what sort of uid it is; the internal id is deliberately
// not in the response at all (invariant 10).
func notificationSubject(n notificationResponse) string {
	if uid, ok := n.Payload["uid"].(string); ok && uid != "" {
		return n.SubjectKind + " " + uid
	}
	return n.SubjectKind
}

func printAlertRule(w io.Writer, r alertRuleResponse, asJSON bool) error {
	if asJSON {
		return writeJSON(w, r)
	}
	state := "active"
	if !r.Active {
		state = "off"
	}
	fmt.Fprintf(w, "%s  %s\n", r.UID, r.Name)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "event\t%s (%s)\n", r.EventType, r.EventName)
	fmt.Fprintf(tw, "channel\t%s\n", r.Channel)
	fmt.Fprintf(tw, "target\t%s\n", r.Target)
	fmt.Fprintf(tw, "state\t%s\n", state)
	fmt.Fprintf(tw, "conditions\t%s\n", conditionSummary(r.Conditions))
	return tw.Flush()
}
