// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

// newAdminCmd is the parent of the administration commands (DESIGN.md 11).
func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administer roles and grants",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newAdminGrantCmd(), newAdminAssignCmd())
	return cmd
}

// newAdminGrantCmd writes a grant.
//
// It is the command face of the anti-escalation rule: the server refuses a
// grant conferring a privilege the caller does not hold over that scope
// (DESIGN.md 7.3), and the refusal arrives here as a 403 with the reason in
// the problem document's detail.
//
// Every scope flag is optional and an omitted one is a wildcard, which is why
// the flags are read through Changed rather than compared to a zero value: a
// grant restricted to site 0 and a grant over every site are different things,
// and only one of them is expressible as an integer.
func newAdminGrantCmd() *cobra.Command {
	var (
		server    string
		asJSON    bool
		role      string
		privilege string
		site      int64
		docKind   string
		state     string
		category  string
		deep      bool
	)
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Give a role a privilege over a scope",
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

			body := map[string]any{"role": role, "privilege": privilege}
			if cmd.Flags().Changed("site") {
				body["site"] = site
			}
			if cmd.Flags().Changed("doc-kind") {
				body["doc_kind"] = docKind
			}
			if cmd.Flags().Changed("state") {
				body["state"] = state
			}
			if cmd.Flags().Changed("category") {
				// The category is named by its path, which is what a person
				// reads and what the grant prefix-matches against. The server
				// resolves it to a row; a client that sent both would be a
				// client that could send two that disagree.
				body["category"] = category
			}
			if cmd.Flags().Changed("deep") {
				body["category_deep"] = deep
			}

			var out struct {
				Privilege   string `json:"privilege"`
				Description string `json:"description"`
			}
			if err := client.Do(cmd.Context(), http.MethodPost, "/api/v1/grants", body, &out); err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(w, out)
			}
			fmt.Fprintf(w, "granted: %s may %s over %s\n", role, out.Privilege, out.Description)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&role, "role", "", "the role slug the grant is for")
	cmd.Flags().StringVar(&privilege, "privilege", "",
		"read, edit, recall, create, publish, or deny")
	cmd.Flags().Int64Var(&site, "site", 0, "restrict the grant to one site")
	cmd.Flags().StringVar(&docKind, "doc-kind", "", "restrict the grant to story, media, or template")
	cmd.Flags().StringVar(&state, "state", "", "restrict the grant to one workflow state")
	cmd.Flags().StringVar(&category, "category", "",
		"restrict the grant to one category, by path; needs --site")
	cmd.Flags().BoolVar(&deep, "deep", true,
		"a category grant covers the whole subtree below it")
	_ = cmd.MarkFlagRequired("role")
	_ = cmd.MarkFlagRequired("privilege")
	return cmd
}

// newAdminAssignCmd gives a user a role.
//
// It carries the same refusal as "admin grant": handing somebody a role hands
// them every grant it carries, so a caller who could not write those grants
// cannot hand out the role either.
func newAdminAssignCmd() *cobra.Command {
	var (
		server string
		user   string
		role   string
	)
	cmd := &cobra.Command{
		Use:   "assign",
		Short: "Give a user a role",
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
			if err := client.Do(cmd.Context(), http.MethodPost,
				"/api/v1/users/"+url.PathEscape(user)+"/roles",
				map[string]string{"role": role}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "assigned: %s now holds %s\n", user, role)
			return nil
		},
	}
	addServerFlag(cmd, &server)
	cmd.Flags().StringVar(&user, "user", "", "the uid of the user to give the role to")
	cmd.Flags().StringVar(&role, "role", "", "the role slug")
	_ = cmd.MarkFlagRequired("user")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}
