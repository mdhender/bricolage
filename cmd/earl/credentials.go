// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Where the token lives (DESIGN.md 11).
//
// One file, mode 0600, under the user's config directory. The directory is not
// created: nothing in this system creates a directory, and the reason is the
// same here as it is for --db (invariant 19) -- a tool that creates what it
// cannot find turns a typo into a second, empty, plausible-looking state.
// "mkdir -p ~/.config/earl" is a line the message prints.
const (
	credentialsDir  = "earl"
	credentialsFile = "credentials.json"
	credentialsMode = 0o600
)

// Credentials are what "earl login" stores and every other command reads.
type Credentials struct {
	Server    string    `json:"server"`
	Email     string    `json:"email"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CredentialsPath returns the file the token is kept in. The override exists
// so that a test does not write to the person running it.
func CredentialsPath() (string, error) {
	if p := os.Getenv("EARL_CREDENTIALS"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the config directory: %w", err)
	}
	return filepath.Join(dir, credentialsDir, credentialsFile), nil
}

// SaveCredentials writes the token, replacing whatever was there.
//
// It writes the file directly rather than through a temporary file and a
// rename, because a rename needs the directory to be writable in a way the
// caller has not been asked about and because a half-written credentials file
// costs one "earl login" to repair.
func SaveCredentials(c Credentials) (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	body = append(body, '\n')

	if err := os.WriteFile(path, body, credentialsMode); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: the directory does not exist; create it yourself:\n    mkdir -p %s",
				path, filepath.Dir(path))
		}
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	// A file that already existed keeps its old mode, so set it explicitly.
	// A token readable by every account on the machine is not a stored
	// credential, it is a published one.
	if err := os.Chmod(path, credentialsMode); err != nil {
		return "", fmt.Errorf("securing %s: %w", path, err)
	}
	return path, nil
}

// LoadCredentials reads the stored token.
func LoadCredentials() (Credentials, error) {
	path, err := CredentialsPath()
	if err != nil {
		return Credentials{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, fmt.Errorf("not signed in: no %s; run \"earl login\"", path)
		}
		return Credentials{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Credentials
	if err := json.Unmarshal(body, &c); err != nil {
		return Credentials{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if c.Token == "" {
		return Credentials{}, fmt.Errorf("%s holds no token; run \"earl login\"", path)
	}
	return c, nil
}
