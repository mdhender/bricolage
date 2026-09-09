// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The invitation and user commands (issue #6).
//
// earl is the acceptance-test harness and the measure of whether the API is
// complete, so everything the UI's invitation screens can do is here: create an
// invitation, list them with the same default the screen has, show one, revoke
// one, redeem one, and find a user's uid.
//
// There is no "extend", no "renew" and no "expire" command, and their absence is
// the design rather than an omission. An administrator cannot see the link they
// would be extending -- it is shown once and what is stored is a hash -- and
// re-inviting an address replaces whatever was pending for it, which is what
// renewing an expired invitation means here. Forcing expiry is "invite revoke"
// with a reason.

const invitationPath = "/api/v1/invitations"

// invitationResponse is one invitation as the API speaks it. There is no token
// in it: the link appears in the response that created it and nowhere else.
type invitationResponse struct {
	UID           string     `json:"uid"`
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	Expired       bool       `json:"expired"`
	Redeemable    bool       `json:"redeemable"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
	SettledAt     *time.Time `json:"settled_at"`
	Reason        string     `json:"reason"`
	InvitedBy     string     `json:"invited_by"`
	InvitedByName string     `json:"invited_by_name"`
	User          string     `json:"user"`
}

type invitationsResponse struct {
	Status      string               `json:"status"`
	Invitations []invitationResponse `json:"invitations"`
}

// createdInvitationResponse carries the link, once.
type createdInvitationResponse struct {
	Invitation invitationResponse `json:"invitation"`
	Link       string             `json:"link"`
	Token      string             `json:"token"`
}

// newInviteCmd is the invitation group.
func newInviteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite",
		Short: "Invite people, and manage the invitations",
		Long: "Invite people, and manage the invitations.\n\n" +
			"Registration is invite-only: there is no sign-up route. An invitation is\n" +
			"good for 48 hours and for one use, and the link is printed once, by\n" +
			"\"invite create\" -- what the server stores is a hash of it, so nothing can\n" +
			"print it again.\n\n" +
			"Inviting an address that already has a pending invitation replaces it:\n" +
			"the new link works and the old one stops. That is how a lapsed\n" +
			"invitation is replaced, and it is why there is no command that extends\n" +
			"one, renews an expired one, or forces one to expire.\n\n" +
			"Creating and revoking need \"create\" over the system; listing needs\n" +
			"\"read\" over it.",
	}
	cmd.AddCommand(newInviteCreateCmd(), newInviteListCmd(), newInviteShowCmd(),
		newInviteRevokeCmd(), newInviteRedeemCmd())
	return cmd
}

// newInviteCreateCmd creates an invitation and prints the link.
func newInviteCreateCmd() *cobra.Command {
	var (
		server string
		email  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Invite an address, printing the link once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out createdInvitationResponse
			if err := client.Do(cmd.Context(), http.MethodPost, invitationPath,
				map[string]string{"email": email}, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "invited %s\n", out.Invitation.Email)
			fmt.Fprintf(w, "invitation: %s\n", out.Invitation.UID)
			fmt.Fprintf(w, "expires:    %s\n", out.Invitation.ExpiresAt.Format(time.RFC3339))
			fmt.Fprintf(w, "link:       %s\n", out.Link)
			fmt.Fprintln(w, "\nSend that link to them. It is shown once, works once, and there is\n"+
				"no e-mail transport yet, so sending it is yours to do.")
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&email, "email", "", "the address to invite")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

// newInviteListCmd lists invitations, pending by default.
func newInviteListCmd() *cobra.Command {
	var (
		server string
		status string
		all    bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List invitations; pending by default",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			path := invitationPath
			switch {
			case all:
				path += "?status=all"
			case status != "":
				path += "?status=" + url.QueryEscape(status)
			}
			var out invitationsResponse
			if err := client.Get(cmd.Context(), path, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			if len(out.Invitations) == 0 {
				fmt.Fprintf(w, "no invitations (%s)\n", out.Status)
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UID\tADDRESS\tSTATUS\tEXPIRES\tINVITED BY\tACCOUNT\tREASON")
			for _, inv := range out.Invitations {
				// "pending" and "expired" are not two statuses: expiry is
				// derived, so a lapsed invitation is a pending row whose
				// deadline has passed and nothing has written anything down.
				state := inv.Status
				if inv.Status == "pending" && inv.Expired {
					state = "pending (expired)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					inv.UID, inv.Email, state, inv.ExpiresAt.Format(time.RFC3339),
					dash(inv.InvitedByName), dash(inv.User), dash(inv.Reason))
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&status, "status", "",
		"pending, redeemed, revoked, or superseded")
	cmd.Flags().BoolVar(&all, "all", false, "every invitation, whatever its status")
	return cmd
}

// newInviteShowCmd shows one invitation.
func newInviteShowCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "show UID",
		Short: "Show one invitation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out invitationResponse
			if err := client.Get(cmd.Context(),
				invitationPath+"/"+url.PathEscape(args[0]), &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "invitation: %s\n", out.UID)
			fmt.Fprintf(w, "address:    %s\n", out.Email)
			fmt.Fprintf(w, "status:     %s\n", out.Status)
			fmt.Fprintf(w, "expires:    %s", out.ExpiresAt.Format(time.RFC3339))
			if out.Expired {
				fmt.Fprint(w, " (passed)")
			}
			fmt.Fprintln(w)
			fmt.Fprintf(w, "redeemable: %t\n", out.Redeemable)
			fmt.Fprintf(w, "invited by: %s\n", dash(out.InvitedByName))
			if out.SettledAt != nil {
				fmt.Fprintf(w, "settled:    %s\n", out.SettledAt.Format(time.RFC3339))
			}
			if out.User != "" {
				fmt.Fprintf(w, "account:    %s\n", out.User)
			}
			if out.Reason != "" {
				fmt.Fprintf(w, "reason:     %s\n", out.Reason)
			}
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newInviteRevokeCmd revokes an invitation.
//
// This is also how an administrator settles a lapsed invitation by hand, and it
// is the nearest thing to "force this to expire" that exists: the effect is
// identical -- the link stops working and the row stays -- and the only thing a
// separate verb would add is a different word in the audit trail, which --reason
// already carries.
func newInviteRevokeCmd() *cobra.Command {
	var (
		server string
		reason string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "revoke UID",
		Short: "Revoke a pending invitation",
		Long: "Revoke a pending invitation.\n\n" +
			"Revoking twice is not an error. Revoking an invitation that was already\n" +
			"redeemed is refused: the account exists, and this would not remove it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			body := map[string]string{}
			if reason != "" {
				body["reason"] = reason
			}
			var out invitationResponse
			if err := client.Do(cmd.Context(), http.MethodPost,
				invitationPath+"/"+url.PathEscape(args[0])+"/revoke", body, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "invitation %s for %s is %s\n", out.UID, out.Email, out.Status)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&reason, "reason", "", "why, for the audit trail")
	return cmd
}

// newInviteRedeemCmd redeems an invitation, which is the one command here that
// needs no credentials.
//
// It issues no session and stores no token, because the route issues none: the
// account is created and the person signs in afterwards. "earl login" is the
// next step, and doing it that way is what keeps the redemption route free of
// login CSRF (issue #6).
func newInviteRedeemCmd() *cobra.Command {
	var (
		server        string
		token         string
		link          string
		email         string
		name          string
		passwordStdin bool
		asJSON        bool
	)
	cmd := &cobra.Command{
		Use:   "redeem",
		Short: "Accept an invitation and create the account",
		Long: "Accept an invitation and create the account.\n\n" +
			"This command needs no credentials: it is how the first session an\n" +
			"account ever has becomes possible. It issues none either -- the account\n" +
			"is created and you sign in with \"earl login\" afterwards, with the\n" +
			"password you set here.\n\n" +
			"The address is required, and it has to be the one that was invited: a\n" +
			"forwarded link is not usable by whoever received it. Every refusal is\n" +
			"the same refusal, because a server that said which of them it was would\n" +
			"be a way to find out who has an account.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if token == "" && link == "" {
				return fmt.Errorf("one of --token or --link is required")
			}
			if link != "" {
				parsed, err := tokenFromLink(link)
				if err != nil {
					return err
				}
				token = parsed
			}
			password, err := readPassword(cmd.InOrStdin(), email, passwordStdin)
			if err != nil {
				return err
			}

			// No credentials, deliberately: this is the one write in the API
			// that an account-less caller makes.
			client, err := NewClient(server, "")
			if err != nil {
				return err
			}
			var out userResponse
			if err := client.Do(cmd.Context(), http.MethodPost, invitationPath+"/redemption",
				map[string]string{
					"token":    token,
					"email":    email,
					"name":     name,
					"password": password,
				}, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "created %s <%s>\n", out.Name, out.Email)
			fmt.Fprintf(w, "uid: %s\n", out.UID)
			fmt.Fprintf(w, "\nNo session was issued. Sign in with:\n  earl login --email %s\n", out.Email)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&token, "token", "", "the token from the invitation link")
	cmd.Flags().StringVar(&link, "link", "", "the whole invitation link, instead of --token")
	cmd.Flags().StringVar(&email, "email", "", "the address that was invited")
	cmd.Flags().StringVar(&name, "name", "", "your name, as it should appear")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false,
		"read the password from stdin instead of prompting")
	_ = cmd.MarkFlagRequired("email")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// tokenFromLink pulls the token out of an invitation link, so that somebody can
// paste what they were sent rather than editing it first.
func tokenFromLink(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("--link %q: %v", link, err)
	}
	token := path.Base(u.Path)
	if token == "" || token == "." || token == "/" {
		return "", fmt.Errorf("--link %q carries no token", link)
	}
	return token, nil
}

// userResponse is one account as the API speaks it.
type userResponse struct {
	UID       string    `json:"uid"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

type usersResponse struct {
	Users []userResponse `json:"users"`
}

// newUserCmd lists accounts and finds one by address.
//
// It exists because "admin assign --user" takes a uid, and before this the only
// uid obtainable was the one "cmsdb bootstrap admin" prints at creation. An API
// that can give a role to a user nobody can find is incomplete.
func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Find accounts and their uids",
		Long: "Find accounts and their uids.\n\n" +
			"Reading the list of accounts is a system-wide question with no document\n" +
			"to scope it to, so it needs \"read\" over the system -- the same rule the\n" +
			"job queue follows.\n\n" +
			"There is nothing here that deactivates an account, removes a role, or\n" +
			"edits a profile: those are issue #7.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newUserListCmd(), newUserShowCmd())
	return cmd
}

func newUserListCmd() *cobra.Command {
	var (
		server string
		query  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List accounts, optionally matching a substring",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			path := "/api/v1/users"
			if query != "" {
				path += "?q=" + url.QueryEscape(query)
			}
			var out usersResponse
			if err := client.Get(cmd.Context(), path, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			if len(out.Users) == 0 {
				fmt.Fprintln(w, "no account matches")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UID\tADDRESS\tNAME\tACTIVE\tCREATED")
			for _, u := range out.Users {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n",
					u.UID, u.Email, u.Name, u.Active, u.CreatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&query, "q", "", "match this substring of an address or a name")
	return cmd
}

func newUserShowCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "show UID",
		Short: "Show one account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := docClient(cmd, server)
			if err != nil {
				return err
			}
			var out userResponse
			if err := client.Get(cmd.Context(),
				"/api/v1/users/"+url.PathEscape(args[0]), &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "uid:     %s\n", out.UID)
			fmt.Fprintf(w, "address: %s\n", out.Email)
			fmt.Fprintf(w, "name:    %s\n", out.Name)
			fmt.Fprintf(w, "active:  %t\n", out.Active)
			fmt.Fprintf(w, "created: %s\n", out.CreatedAt.Format(time.RFC3339))
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}
