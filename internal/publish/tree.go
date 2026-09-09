// Copyright (c) 2026 Michael D Henderson.

package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
)

// The output tree (DESIGN.md 8.3, 8.4, PLAN.md M9).
//
// This is the one place in the system that creates a directory, and it is
// worth saying exactly why the rule bends here and nowhere else.
//
// Invariant 19 exists so that no tool creates the directory it was told to
// use. "--db /var/lib/cms" that does not exist is a typo, and a tool that
// makes it turns the typo into a plausible-looking, empty CMS somebody finds
// out about hours later. That property is kept here in full: the output root
// must already exist, NewTree refuses one that does not, and cmsd opens it
// while starting so a mistyped --output is a refusal at startup rather than at
// the first publish.
//
// What is created is the interior of that tree, and the interior is not
// configuration. "/features/film/2026/03/01/a-feature/" is derived from a
// category path, a URI format, and a cover date -- it is content, computed,
// and there is no typo it could be. The alternative is not "no directories":
// it is an output tree that is not a tree, which is a publishing system whose
// output no web server can serve, and DESIGN.md 8.4 already describes the
// output tree as having exactly this shape when it explains why the preview
// tree cannot.
//
// Two things keep the exception narrow. Every path is validated by
// domain.OutputPath before it gets here, and every operation goes through an
// os.Root opened on the output directory, so a category named "../../etc"
// cannot address a byte outside the tree even if the first check were wrong.

// The permissions a published tree is written with.
//
// World-readable, because the point of the tree is that a web server running
// as somebody else reads it, and not world-writable, because the point of a
// publishing system is that only it writes. Neither is configurable: a
// permission that is a flag is a permission that is wrong on one machine.
const (
	dirPerm  fs.FileMode = 0o755
	filePerm fs.FileMode = 0o644
)

// Tree is the output tree: the directory published files are written beneath.
type Tree struct {
	dir  string
	root *os.Root
}

// NewTree opens an output tree.
//
// The directory must already exist. See the note above for why that half of
// invariant 19 is kept exactly as it is while the interior of the tree is
// created.
func NewTree(dir string) (*Tree, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("publish: no output directory: %w", domain.ErrUnavailable)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("output directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("output directory %s: not a directory", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("output directory %s: %w", dir, err)
	}
	return &Tree{dir: dir, root: root}, nil
}

// Dir is the directory the tree was opened from, for a log line and for the
// message that says where a file went.
func (t *Tree) Dir() string { return t.dir }

// Close releases the tree's handle on its root.
func (t *Tree) Close() error {
	if t == nil || t.root == nil {
		return nil
	}
	return t.root.Close()
}

// Files returns every file in the tree, as paths relative to the root, sorted.
//
// It is what "cmsdb check" compares the resource rows against
// (PLAN.md M9 acceptance 7). Directories are not returned: an empty directory
// is not an orphan, it is what is left after the last file under an address
// expired, and reporting it would make every slug change look like damage.
func (t *Tree) Files() ([]string, error) {
	var out []string
	err := fs.WalkDir(t.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || p == "." {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking the output tree %s: %w", t.dir, err)
	}
	sort.Strings(out)
	return out, nil
}

// Read returns the bytes at a path within the tree.
func (t *Tree) Read(p string) ([]byte, error) {
	b, err := t.root.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", p, domain.ErrNotFound)
		}
		return nil, err
	}
	return b, nil
}

// Remove deletes the file at p. A file that is not there is not an error:
// expiry is idempotent, because a job may be retried after the delete
// succeeded and the process died before it could say so.
func (t *Tree) Remove(p string) error {
	err := t.root.Remove(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting %s from the output tree: %w", p, err)
	}
	return nil
}

// Written is one file a batch put into the tree.
type Written struct {
	Path     string
	Checksum string
	Bytes    int64
}

// Batch is a set of writes that succeed or are undone together.
//
// It is what makes "a publish failure leaves no partial output"
// (PLAN.md M9 acceptance 6) a property of the shape rather than of a cleanup
// somebody has to remember. Put records enough to undo itself -- whether the
// file was there before, and its bytes if it was -- and Rollback puts the tree
// back the way it found it.
//
// The previous bytes are held in memory. A publish is one document across a
// handful of channels and a page is a page; a design that held a whole
// republish would not do this, and would not need to, because it would be a
// batch per document.
type Batch struct {
	tree *Tree

	// order is the paths in the order they were written, so Rollback undoes
	// them in reverse.
	order []string

	// created are the paths that did not exist before, and replaced holds the
	// previous bytes of the ones that did.
	created  map[string]bool
	replaced map[string][]byte
}

// Begin starts a batch of writes.
func (t *Tree) Begin() *Batch {
	return &Batch{tree: t, created: map[string]bool{}, replaced: map[string][]byte{}}
}

// Put writes body at p, creating the directories above it.
//
// The write goes through a temporary file in the same directory and a rename,
// so a reader sees the whole page or the previous one: a published file is
// something a web server may be reading at the moment it is replaced, and a
// truncate-and-write would serve half a page to whoever asked in between.
func (b *Batch) Put(p string, body []byte) (Written, error) {
	if err := validTreePath(p); err != nil {
		return Written{}, err
	}
	if _, seen := b.created[p]; seen {
		return Written{}, fmt.Errorf("output path %s: written twice in one publish: %w", p, domain.ErrConflict)
	}
	if _, seen := b.replaced[p]; seen {
		return Written{}, fmt.Errorf("output path %s: written twice in one publish: %w", p, domain.ErrConflict)
	}

	// Remember what was there, so this is undoable.
	switch previous, err := b.tree.root.ReadFile(p); {
	case err == nil:
		b.replaced[p] = previous
	case errors.Is(err, fs.ErrNotExist):
		b.created[p] = true
	default:
		return Written{}, fmt.Errorf("reading %s before replacing it: %w", p, err)
	}

	if err := b.tree.mkdirAll(path.Dir(p)); err != nil {
		return Written{}, err
	}
	if err := b.tree.write(p, body); err != nil {
		return Written{}, err
	}
	b.order = append(b.order, p)

	sum := sha256.Sum256(body)
	return Written{Path: p, Checksum: hex.EncodeToString(sum[:]), Bytes: int64(len(body))}, nil
}

// Rollback undoes every write in the batch, newest first.
//
// It reports the first failure and keeps going, because a rollback that stops
// at the first problem leaves more behind than one that does not, and the
// caller is already returning an error either way.
func (b *Batch) Rollback() error {
	var errs []error
	for i := len(b.order) - 1; i >= 0; i-- {
		p := b.order[i]
		if previous, ok := b.replaced[p]; ok {
			if err := b.tree.write(p, previous); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := b.tree.Remove(p); err != nil {
			errs = append(errs, err)
		}
	}
	b.order = nil
	return errors.Join(errs...)
}

// mkdirAll creates the directories above a file. It is the one directory
// creation in this system, and the note at the top of this file is why.
func (t *Tree) mkdirAll(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if err := t.root.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating %s in the output tree: %w", dir, err)
	}
	return nil
}

// write puts body at p through a temporary file and a rename.
func (t *Tree) write(p string, body []byte) error {
	tmp := path.Join(path.Dir(p), tempName(p))
	f, err := t.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("writing %s to the output tree: %w", p, err)
	}
	defer func() { _ = t.root.Remove(tmp) }() // a no-op once the rename has happened

	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s to the output tree: %w", p, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s to the output tree: %w", p, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s to the output tree: %w", p, err)
	}
	if err := t.root.Rename(tmp, p); err != nil {
		return fmt.Errorf("writing %s to the output tree: %w", p, err)
	}
	return nil
}

// tempName is the temporary file a write goes through, in the same directory
// as its destination so that the rename stays within one filesystem.
//
// It is derived from the destination rather than random, so that two
// concurrent publishes of the same address collide on O_EXCL instead of both
// renaming over each other: the loser fails and its job is retried, which is
// what a queue is for.
func tempName(p string) string { return ".publish-" + strings.ReplaceAll(path.Base(p), "/", "_") }

// validTreePath refuses a path that would leave the tree or that names a
// temporary file.
//
// The os.Root beneath every operation makes the first refusal redundant, and
// it is here anyway: a message naming the path is worth more than one naming a
// syscall, and the two checks fail for different reasons.
func validTreePath(p string) error {
	if p == "" || p != path.Clean(p) || path.IsAbs(p) {
		return fmt.Errorf("output path %q: not a clean relative path: %w", p, domain.ErrInvalid)
	}
	for _, segment := range strings.Split(p, "/") {
		switch {
		case segment == "" || segment == "." || segment == "..":
			return fmt.Errorf("output path %q: %q is not a path segment: %w", p, segment, domain.ErrInvalid)
		case strings.HasPrefix(segment, ".publish-"):
			return fmt.Errorf("output path %q: %q is the name this system writes through: %w",
				p, segment, domain.ErrInvalid)
		}
	}
	if filepath.Separator != '/' && strings.ContainsRune(p, filepath.Separator) {
		return fmt.Errorf("output path %q: contains a path separator this system does not write: %w",
			p, domain.ErrInvalid)
	}
	return nil
}
