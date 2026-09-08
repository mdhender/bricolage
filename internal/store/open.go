// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/mdhender/bricolage/internal/migrate"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Open flags are written out in full at every open, and only the create path
// names OpenCreate.
//
// This is not style. The zero value of sqlitex.PoolOptions.Flags and the
// no-flag form of sqlite.OpenConn both mean
// OpenReadWrite|OpenCreate|OpenWAL|OpenURI — they create. An open that forgets
// to say what it wants is an open that brings a database into existence, and
// a server that creates a database is a server that comes up healthy and
// empty (DESIGN.md 13.3).
//
// OpenURI is deliberately absent from the file paths: with it, a directory
// containing "?" would be read as a URI with query parameters. The in-memory
// store is the only thing here that opens a URI, and it says so.
const (
	createFlags = sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenWAL
	writeFlags  = sqlite.OpenReadWrite | sqlite.OpenWAL
	readFlags   = sqlite.OpenReadOnly

	memoryWriteFlags = sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenURI
	memoryReadFlags  = sqlite.OpenReadOnly | sqlite.OpenURI
)

// VersionPolicy says what a caller makes of a database whose schema version is
// behind the binary.
//
// A database ahead of the binary is always an error, under either policy: the
// binary is older than the database, and no amount of migrating forward
// repairs that.
type VersionPolicy int

const (
	// RequireExact rejects anything but an exact match. This is cmsd
	// (invariant 21) and every cmsdb subcommand that reads the schema.
	RequireExact VersionPolicy = iota

	// AllowBehind accepts a database with migrations pending, so that
	// "cmsdb migrate status" can report them and "cmsdb migrate up" can apply
	// them. Nothing else uses it.
	AllowBehind
)

// Options are the knobs on Open. The zero value is what cmsd wants: an exact
// version match and the defaults for the pool.
type Options struct {
	// Version says whether a pending migration is an error.
	Version VersionPolicy

	// ReadPoolSize is the number of reader connections; zero means
	// DefaultReadPoolSize.
	ReadPoolSize int

	// BusyTimeout is the per-connection lock timeout; zero means
	// DefaultBusyTimeout.
	BusyTimeout time.Duration
}

func (o Options) readPoolSize() int {
	if o.ReadPoolSize > 0 {
		return o.ReadPoolSize
	}
	return DefaultReadPoolSize
}

func (o Options) busyTimeout() time.Duration {
	if o.BusyTimeout > 0 {
		return o.BusyTimeout
	}
	return DefaultBusyTimeout
}

// Path returns the database file inside dir. The file name is a constant, so
// there is one answer per directory (DESIGN.md 13.1).
func Path(dir string) string { return filepath.Join(dir, dbFileName) }

// Exists reports whether dir holds a database. It returns a *DirError if dir
// itself is missing or is not a directory.
func Exists(dir string) (bool, error) {
	if err := checkDir(dir); err != nil {
		return false, err
	}
	switch _, err := os.Stat(Path(dir)); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// checkDir requires that dir exists and is a directory.
//
// This is the first thing every entry point does, and it is the reason the
// standard library's directory-creating calls appear nowhere in this
// repository — a rule "make lint" enforces with a grep (invariant 19). A tool
// that creates what it cannot find turns "cmsd serve --db ./vsr" into a
// plausible-looking, empty system, and the mistake surfaces hours later as
// "where did everything go".
func checkDir(dir string) error {
	if dir == "" {
		return &DirError{Dir: dir, Err: errors.New("no directory given")}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return &DirError{Dir: dir, Err: err}
	}
	if !info.IsDir() {
		return &DirError{Dir: dir, Err: errors.New("not a directory")}
	}
	return nil
}

// Create brings a database into existence in dir and applies every embedded
// migration to it.
//
// This is the only function in this repository that names OpenCreate against a
// file, and "cmsdb init" is its only caller (invariant 20). It fails if dir
// does not exist and it never creates one. It fails if the database already
// exists, because creating something twice is not creation; "cmsdb init"
// turns that into the "already initialised" report.
func Create(ctx context.Context, dir string) (*DB, error) {
	exists, err := Exists(dir)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, &ExistsError{Path: Path(dir)}
	}

	path := Path(dir)
	conn, err := sqlite.OpenConn(path, createFlags)
	if err != nil {
		return nil, fmt.Errorf("creating %q: %w", path, err)
	}
	if err := prepareConn(conn, DefaultBusyTimeout); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("creating %q: %w", path, err)
	}
	// migrate.Apply stamps the application ID and advances user_version once
	// per migration, inside that migration's transaction.
	if err := migrate.Apply(ctx, conn, -1); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("creating %q: %w", path, err)
	}
	if err := conn.Close(); err != nil {
		return nil, fmt.Errorf("creating %q: %w", path, err)
	}

	// Reopen through the ordinary path, so that what init hands back has been
	// through the same verification as what cmsd opens. A create that produced
	// something Open would reject is a bug worth finding here.
	return Open(ctx, dir, Options{})
}

// Open opens the database in dir and verifies it.
//
// It never names OpenCreate, so a missing file is SQLITE_CANTOPEN rather than
// a new, empty database. It checks the application ID and the schema version
// on the way in, and returns one of the typed errors in errors.go for each of
// the four failures in DESIGN.md 13.4.
func Open(ctx context.Context, dir string, opts Options) (*DB, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	path := Path(dir)

	conn, err := sqlite.OpenConn(path, writeFlags)
	if err != nil {
		if sqlite.ErrCode(err).ToPrimary() == sqlite.ResultCantOpen {
			// Detected by result code, never by matching the message
			// (invariant 11).
			return nil, &NotFoundError{Dir: dir, Path: path, Err: err}
		}
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	if err := prepareConn(conn, opts.busyTimeout()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}

	version, err := verify(conn, dir, path, opts.Version)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	// WAL on every persistent store (invariant 22). The OpenWAL flag sets it;
	// reading it back is how we learn that it took, on a file somebody left in
	// another journal mode.
	if mode, err := pragmaText(conn, "journal_mode"); err != nil {
		_ = conn.Close()
		return nil, err
	} else if mode != "wal" {
		_ = conn.Close()
		return nil, fmt.Errorf("database %q: journal_mode is %q, expected \"wal\"", path, mode)
	}

	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{
		Flags:       readFlags,
		PoolSize:    opts.readPoolSize(),
		PrepareConn: func(c *sqlite.Conn) error { return prepareConn(c, opts.busyTimeout()) },
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("opening readers for %q: %w", path, err)
	}

	db := &DB{path: path, reads: pool, write: conn}
	db.version.Store(version)
	return db, nil
}

// memoryCounter names in-memory databases apart. Two stores in one process
// must not share a cache by accident, which is exactly what a fixed name would
// do.
var memoryCounter atomic.Uint64

// OpenMemory creates an in-memory database with every migration applied.
//
// It exists for tests, and it is a real store rather than a mock: the same
// migrations through the same runner, the same pool shape, and foreign keys on
// every connection (invariant 22). A test that passes with foreign keys off
// proves nothing.
//
// There is no WAL here and none is asked for: an in-memory database has no
// journal file, and requesting the mode is a no-op at best.
func OpenMemory(ctx context.Context) (*DB, error) {
	// Shared cache is what lets several connections see one in-memory
	// database. It is not recommended for files and is the documented way to
	// do this for memory.
	uri := fmt.Sprintf("file:cms-memory-%d?mode=memory&cache=shared", memoryCounter.Add(1))

	conn, err := sqlite.OpenConn(uri, memoryWriteFlags)
	if err != nil {
		return nil, fmt.Errorf("opening an in-memory database: %w", err)
	}
	if err := prepareConn(conn, DefaultBusyTimeout); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := migrate.Apply(ctx, conn, -1); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("migrating an in-memory database: %w", err)
	}
	version, err := verify(conn, "", uri, RequireExact)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	pool, err := sqlitex.NewPool(uri, sqlitex.PoolOptions{
		Flags:       memoryReadFlags,
		PoolSize:    DefaultReadPoolSize,
		PrepareConn: func(c *sqlite.Conn) error { return prepareConn(c, DefaultBusyTimeout) },
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("opening readers for an in-memory database: %w", err)
	}

	db := &DB{path: uri, reads: pool, write: conn}
	db.version.Store(version)
	return db, nil
}

// verify performs the application ID and schema version checks from
// DESIGN.md 13.4 and returns the version it read.
//
// The application ID check is ours rather than sqlitemigration's on purpose.
// sqlitemigration accepts an ID of 0 when the database has no schema, so that
// it can adopt a freshly created empty file. That is right for "cmsdb init"
// and wrong for everything else: an empty file is exactly what a mistyped path
// produces, and adopting it is how a server comes up healthy and empty
// (DESIGN.md 13.2, invariant 21).
func verify(conn *sqlite.Conn, dir, path string, policy VersionPolicy) (int32, error) {
	appID, err := pragmaInt32(conn, "application_id")
	if err != nil {
		return 0, err
	}
	if appID != migrate.AppID {
		return 0, &AppIDError{Path: path, Want: migrate.AppID, Got: appID}
	}

	version, err := pragmaInt32(conn, "user_version")
	if err != nil {
		return 0, err
	}
	want := int32(migrate.Count())
	switch {
	case version == want:
	case version < want && policy == AllowBehind:
	default:
		return 0, &VersionError{Dir: dir, Path: path, Want: want, Got: version}
	}
	return version, nil
}

// Migrate applies pending migrations up to version to, and returns the version
// before and after. A negative to means every embedded migration.
//
// Only cmsdb reaches this. cmsd opens and verifies and does not own a way to
// say yes: a server that migrates on startup upgrades a production database
// because somebody restarted it (DESIGN.md 13.4, invariant 20).
func (db *DB) Migrate(ctx context.Context, to int) (before, after int32, err error) {
	err = db.Write(ctx, func(conn *sqlite.Conn) error {
		v, err := pragmaInt32(conn, "user_version")
		if err != nil {
			return err
		}
		before = v
		if err := migrate.Apply(ctx, conn, to); err != nil {
			return err
		}
		after, err = pragmaInt32(conn, "user_version")
		return err
	})
	if err != nil {
		return before, after, fmt.Errorf("migrating %q: %w", db.path, err)
	}
	db.version.Store(after)
	return before, after, nil
}
