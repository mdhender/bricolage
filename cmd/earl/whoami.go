// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// meResponse is GET /api/v1/me.
type meResponse struct {
	User struct {
		UID       string    `json:"uid"`
		Email     string    `json:"email"`
		Name      string    `json:"name"`
		Active    bool      `json:"active"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"user"`
	Roles []struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"roles"`
	Grants []struct {
		Privilege   string `json:"privilege"`
		Description string `json:"description"`
	} `json:"grants"`
	ExpiresAt time.Time `json:"session_expires_at"`
}

// newWhoamiCmd prints who the stored token belongs to.
//
// It prints the roles and the grants as well as the user, because that is what
// makes "the session carries exactly this user's roles and grants" something a
// person can check by looking rather than by reading the code.
func newWhoamiCmd() *cobra.Command {
	var (
		server string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Print who the stored token signs in as",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			creds, err := LoadCredentials()
			if err != nil {
				return err
			}
			// The stored server wins unless --server was given, so that
			// "earl login" then "earl whoami" needs no flags.
			if !cmd.Flags().Changed("server") && creds.Server != "" {
				server = creds.Server
			}

			client, err := NewClient(server, creds.Token)
			if err != nil {
				return err
			}

			var me meResponse
			if err := client.Get(cmd.Context(), "/api/v1/me", &me); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, me)
			}

			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "email\t%s\n", me.User.Email)
			fmt.Fprintf(tw, "name\t%s\n", me.User.Name)
			fmt.Fprintf(tw, "uid\t%s\n", me.User.UID)
			fmt.Fprintf(tw, "active\t%t\n", me.User.Active)
			fmt.Fprintf(tw, "server\t%s\n", client.Server)
			fmt.Fprintf(tw, "session expires\t%s\n", me.ExpiresAt.Format(time.RFC3339))
			if len(me.Roles) == 0 {
				fmt.Fprintf(tw, "roles\t(none)\n")
			}
			for i, r := range me.Roles {
				label := "roles"
				if i > 0 {
					label = ""
				}
				fmt.Fprintf(tw, "%s\t%s (%s)\n", label, r.Slug, r.Name)
			}
			if len(me.Grants) == 0 {
				fmt.Fprintf(tw, "grants\t(none)\n")
			}
			for i, g := range me.Grants {
				label := "grants"
				if i > 0 {
					label = ""
				}
				fmt.Fprintf(tw, "%s\t%s over %s\n", label, g.Privilege, g.Description)
			}
			return tw.Flush()
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newLogoutCmd deletes the session and forgets the token.
func newLogoutCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "End the session and forget the token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			creds, err := LoadCredentials()
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("server") && creds.Server != "" {
				server = creds.Server
			}
			client, err := NewClient(server, creds.Token)
			if err != nil {
				return err
			}
			if err := client.Do(cmd.Context(), "DELETE", "/api/v1/sessions/current", nil, nil); err != nil {
				return err
			}
			path, err := SaveCredentials(Credentials{Server: client.Server})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed out; %s no longer holds a token\n", path)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	return cmd
}
