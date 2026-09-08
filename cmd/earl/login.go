// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// devLoginPath is the development login route (DESIGN.md 11). It is written
// out here rather than imported from internal/web/devroutes because earl is a
// client: it knows the API's URLs the way any other client does, and a client
// that imports the server's route constants is a client that cannot be pointed
// at a different build.
const devLoginPath = "/__development/log-me-in/"

// loginResponse is the body of POST /api/v1/sessions.
type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      struct {
		UID   string `json:"uid"`
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
}

func newLoginCmd() *cobra.Command {
	var (
		server        string
		email         string
		dev           bool
		passwordStdin bool
		asJSON        bool
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in and store the token",
		Long: "Sign in and store the token.\n\n" +
			"--dev uses the development login route, which needs no password and\n" +
			"requires the server to be running with --env development. It is a\n" +
			"client-side flag: it selects the route, and the server decides.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := NewClient(server, "")
			if err != nil {
				return err
			}

			var result loginResponse
			if dev {
				result, err = devLogin(cmd.Context(), client, email)
			} else {
				result, err = passwordLogin(cmd.Context(), client, cmd.InOrStdin(), email, passwordStdin)
			}
			if err != nil {
				return err
			}

			path, err := SaveCredentials(Credentials{
				Server:    client.Server,
				Email:     result.User.Email,
				Token:     result.Token,
				ExpiresAt: result.ExpiresAt,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{
					"server":      client.Server,
					"email":       result.User.Email,
					"uid":         result.User.UID,
					"expires_at":  result.ExpiresAt,
					"credentials": path,
				})
			}
			fmt.Fprintf(out, "signed in to %s as %s <%s>\n", client.Server, result.User.Name, result.User.Email)
			fmt.Fprintf(out, "token stored in %s, expires %s\n", path, result.ExpiresAt.Format(time.RFC3339))
			return nil
		},
	}
	addServerFlag(cmd, &server)
	cmd.Flags().StringVar(&email, "email", "", "the email address to sign in as")
	cmd.Flags().BoolVar(&dev, "dev", false,
		"use the development login route; no password, and the server must be in development")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false,
		"read the password from stdin instead of prompting")
	addJSONFlag(cmd, &asJSON)
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

// passwordLogin is POST /api/v1/sessions.
//
// The password is prompted for, or read from stdin. It is never a flag, for
// the same reason "cmsdb bootstrap admin" refuses one: arguments are visible
// in "ps" and land in shell history.
func passwordLogin(ctx context.Context, client *Client, stdin io.Reader, email string, fromStdin bool) (loginResponse, error) {
	password, err := readPassword(stdin, email, fromStdin)
	if err != nil {
		return loginResponse{}, err
	}
	var result loginResponse
	err = client.Do(ctx, http.MethodPost, "/api/v1/sessions", map[string]string{
		"email":    email,
		"password": password,
	}, &result)
	return result, err
}

// devLogin uses GET /__development/log-me-in/{email}.
//
// The flag is client-side: it selects this route, and the server decides
// whether it exists. A server that is not in development has never registered
// it, so the answer is a 404 -- and the message says what that means, because
// "404" on its own sends somebody looking for a typo in the email.
func devLogin(ctx context.Context, client *Client, email string) (loginResponse, error) {
	body, err := client.GetText(ctx, devLoginPath+url.PathEscape(email))
	if err != nil {
		var p *Problem
		if errors.As(err, &p) && p.Status == http.StatusNotFound {
			return loginResponse{}, fmt.Errorf(
				"%s has no development login route, or no user %q.\n"+
					"The route exists only when the server runs with --env development or CMS_ENV=development;\n"+
					"the user must already exist -- run \"cmsdb bootstrap admin\" first.",
				client.Server, email)
		}
		return loginResponse{}, err
	}

	var result loginResponse
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		// The route answers plain text when nothing asked for JSON. earl
		// always asks, so this is a server that is older or a proxy that
		// rewrote the Accept header; take the token and carry on.
		result.Token = strings.TrimSpace(body)
		result.User.Email = email
		result.User.Name = email
	}
	if result.Token == "" {
		return loginResponse{}, fmt.Errorf("%s returned no token", client.Server)
	}
	return result, nil
}

// readPassword returns the password to log in with.
func readPassword(stdin io.Reader, email string, fromStdin bool) (string, error) {
	if fromStdin {
		b, err := io.ReadAll(io.LimitReader(stdin, 4096))
		if err != nil {
			return "", fmt.Errorf("reading the password from stdin: %w", err)
		}
		p := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if p == "" {
			return "", fmt.Errorf("--password-stdin was given but stdin was empty")
		}
		return p, nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("no terminal to prompt on; use --password-stdin, or --dev against a development server")
	}
	fmt.Fprintf(os.Stderr, "password for %s: ", email)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the password: %w", err)
	}
	return string(b), nil
}

// addServerFlag gives a command the --server flag.
//
// The default is the public origin, not the Go listener: talking to
// 127.0.0.1:18443 directly bypasses the proxy and exercises a path that does
// not exist in production (DESIGN.md 11).
func addServerFlag(cmd *cobra.Command, server *string) {
	def := config.DefaultPublicOrigin
	if v := os.Getenv("EARL_SERVER"); v != "" {
		def = v
	}
	cmd.Flags().StringVar(server, "server", def, "the server's public origin (also $EARL_SERVER)")
}

// addJSONFlag gives a command --json. Every command supports it
// (DESIGN.md 11).
func addJSONFlag(cmd *cobra.Command, asJSON *bool) {
	cmd.Flags().BoolVar(asJSON, "json", false, "machine-readable output")
}

// writeJSON renders v as indented JSON, which is what --json means here: a
// person reads it as often as a program does.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
