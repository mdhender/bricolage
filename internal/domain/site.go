// Copyright (c) 2026 Michael D Henderson.

package domain

// Site is one publication (DESIGN.md 5.3).
//
// It is the outermost scope dimension of a grant and the domain an output
// channel's URLs resolve against. Every site has exactly one root category,
// created in the same transaction as the site itself, so that path arithmetic
// is total: a document filed nowhere in particular is filed at "/".
type Site struct {
	ID     int64
	UID    string
	Name   string
	Domain string
	Active bool
}

// SubjectSite is the events subject kind for a site.
const SubjectSite = "site"
