// Copyright (c) 2026 Michael D Henderson.

package publish

import (
	"context"
	"sort"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/store"
)

// Reconciling the output tree with what the database says is in it
// (PLAN.md M9 acceptance 7).
//
// Two failures are worth a line in a check that runs in a cron job, and they
// are opposite mistakes. A resource row whose file is gone is a page the
// system believes it is serving and is not -- somebody deleted it, or a disk
// filled, and nothing will put it back because a republish of an unchanged
// document produces the same address and the same bytes. A file with no row is
// a page nothing will ever expire: it survives every slug change, every
// refiling, and every delete, and it is exactly what a system without
// published_resources leaves behind everywhere.
//
// The comparison is between two sorted lists of paths, which is why the store
// returns resources in path order and the tree walk sorts.

// Difference is what a reconciliation found.
type Difference struct {
	// Missing are resource rows whose file is not in the output tree, by
	// path.
	Missing []string

	// Unknown are files in the output tree that no resource row claims, by
	// path.
	Unknown []string
}

// Empty reports whether the tree and the database agree.
func (d Difference) Empty() bool { return len(d.Missing) == 0 && len(d.Unknown) == 0 }

// Count is how many discrepancies there are in total.
func (d Difference) Count() int { return len(d.Missing) + len(d.Unknown) }

// Reconcile compares the resource rows with the files in the tree.
func Reconcile(ctx context.Context, db *store.DB, tree *Tree) (Difference, error) {
	resources, err := db.AllResources(ctx)
	if err != nil {
		return Difference{}, err
	}
	files, err := tree.Files()
	if err != nil {
		return Difference{}, err
	}
	return Compare(resources, files), nil
}

// Compare is the pure half: the set difference between the paths the rows name
// and the paths on disk.
//
// It is separate from Reconcile so that the rule can be tested without a
// database and without a filesystem, which is the same split every other
// decision in this system gets.
func Compare(resources []domain.Resource, files []string) Difference {
	claimed := make(map[string]struct{}, len(resources))
	for _, r := range resources {
		claimed[r.Path] = struct{}{}
	}
	present := make(map[string]struct{}, len(files))
	for _, f := range files {
		present[f] = struct{}{}
	}

	var d Difference
	for path := range claimed {
		if _, ok := present[path]; !ok {
			d.Missing = append(d.Missing, path)
		}
	}
	for path := range present {
		if _, ok := claimed[path]; !ok {
			d.Unknown = append(d.Unknown, path)
		}
	}
	sort.Strings(d.Missing)
	sort.Strings(d.Unknown)
	return d
}
