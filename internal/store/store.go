// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// dbFileName is the database inside the --db directory. It is a constant and
// not a flag, not a configuration key, and not a parameter, so that --db
// cannot address two different files depending on which command was typed
// (DESIGN.md 13.1).
const dbFileName = "cms.db"

// DefaultReadPoolSize is the number of reader connections. Readers are cheap
// under WAL, where they do not block the writer and the writer does not block
// them.
const DefaultReadPoolSize = 8

// DefaultBusyTimeout is how long a connection waits for a lock before giving
// up. It is set on every connection of every store (DESIGN.md 13.3).
//
// Under this package's own rules a writer never waits on another writer in the
// same process — there is exactly one write connection and it is serialized —
// so the timeout is here for the other process holding the file: a cmsdb
// running beside a cmsd.
const DefaultBusyTimeout = 5 * time.Second

// DB is an open database: a pool of readers and one writer.
//
// The single writer is the point (DESIGN.md 13.3). SQLite permits one write
// transaction at a time whatever we do; letting N goroutines open one and
// hoping busy_timeout sorts it out converts a queue we control into SQLITE_BUSY
// errors we do not. Serializing on a mutex in front of one connection makes
// the queue explicit and the failure mode disappear.
type DB struct {
	path  string
	reads *sqlitex.Pool

	// writeMu serializes the single write connection. Writes queue here rather
	// than contending inside SQLite.
	writeMu sync.Mutex
	write   *sqlite.Conn

	// version is PRAGMA user_version as it was when the database was opened
	// and verified, or as Migrate last left it. Nothing else changes it while
	// a DB is open: cmsd never migrates, so the value a server reads is the
	// value it verified at startup.
	version atomic.Int32

	closeOnce sync.Once
	closeErr  error
}

// Path returns the database file this DB opened. It is the directory that was
// asked for joined to the constant file name, and it is what every error
// message names.
func (db *DB) Path() string { return db.path }

// SchemaVersion returns PRAGMA user_version as read at open time, or as
// Migrate last left it.
func (db *DB) SchemaVersion() int32 { return db.version.Load() }

// Read runs fn with a reader connection from the pool.
//
// The connection is read-only at the SQLite level, so a write that reaches it
// by mistake fails rather than racing the writer. Cancelling ctx interrupts
// the query.
func (db *DB) Read(ctx context.Context, fn func(conn *sqlite.Conn) error) error {
	conn, err := db.reads.Take(ctx)
	if err != nil {
		return fmt.Errorf("taking a reader: %w", err)
	}
	defer db.reads.Put(conn)
	return fn(conn)
}

// Write runs fn with the single write connection, serialized against every
// other writer in this process.
//
// fn owns its own transactions. This method promises only that no other write
// is in flight while it runs, which is what makes "one writer" true rather
// than aspirational.
func (db *DB) Write(ctx context.Context, fn func(conn *sqlite.Conn) error) error {
	// Honour cancellation before queueing behind other writers, so that a
	// cancelled request does not wait for a lock it will not use.
	if err := ctx.Err(); err != nil {
		return err
	}

	db.writeMu.Lock()
	defer db.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	defer db.write.SetInterrupt(db.write.SetInterrupt(ctx.Done()))
	return fn(db.write)
}

// Tx runs fn inside one immediate write transaction on the single write
// connection, committing when fn returns nil and rolling back otherwise.
//
// It exists because a state change and the event recording it are one change
// (invariant 7). An event that can be rolled back separately from the row it
// describes is not an audit record, and a row whose event was rolled back is a
// change nobody can account for. Everything in this package that writes more
// than one row goes through here.
//
// IMMEDIATE rather than DEFERRED: the write lock is taken at BEGIN rather than
// at the first write, so a transaction that will contend does so before it has
// read anything it would have to re-read.
func (db *DB) Tx(ctx context.Context, fn func(conn *sqlite.Conn) error) error {
	return db.Write(ctx, func(conn *sqlite.Conn) (err error) {
		end, beginErr := sqlitex.ImmediateTransaction(conn)
		if beginErr != nil {
			return fmt.Errorf("beginning a transaction: %w", beginErr)
		}
		// The closure's return is named so that end, which commits or rolls
		// back depending on what it points at and may report a failure of its
		// own, can reach the value being returned. An unnamed return here
		// would be evaluated before the defer ran, and a failed commit would
		// be reported as success.
		defer end(&err)
		err = fn(conn)
		return err
	})
}

// Close closes the writer and the reader pool. It is idempotent, so a
// command's defer and a server's shutdown may both call it.
func (db *DB) Close() error {
	db.closeOnce.Do(func() {
		var errs []error
		db.writeMu.Lock()
		if db.write != nil {
			// Clear any interrupt left by a cancelled Write; a conn closed
			// while interrupted reports the interrupt instead of closing.
			db.write.SetInterrupt(nil)
			errs = append(errs, db.write.Close())
			db.write = nil
		}
		db.writeMu.Unlock()
		if db.reads != nil {
			errs = append(errs, db.reads.Close())
			db.reads = nil
		}
		db.closeErr = errors.Join(errs...)
	})
	return db.closeErr
}

// prepareConn applies the per-connection settings to every connection of every
// store, persistent and in-memory alike (invariant 22, DESIGN.md 13.3).
//
// Both settings are properties of a connection rather than of the file, so one
// connection that skips them is silently wrong for its whole life while the
// rest of the process looks correct. foreign_keys is read back rather than
// assumed: a build of SQLite compiled without foreign key support accepts the
// pragma and ignores it, and referential integrity that is off is worth
// failing to start over.
func prepareConn(conn *sqlite.Conn, busy time.Duration) error {
	ms := busy.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA busy_timeout = %d;", ms), nil); err != nil {
		return fmt.Errorf("setting busy_timeout: %w", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = ON;", nil); err != nil {
		return fmt.Errorf("setting foreign_keys: %w", err)
	}
	on, err := pragmaBool(conn, "foreign_keys")
	if err != nil {
		return err
	}
	if !on {
		return errors.New("PRAGMA foreign_keys is off after being set on; this build of SQLite has foreign keys compiled out (invariant 22)")
	}
	return nil
}

// pragmaInt32 reads a single-value integer pragma.
func pragmaInt32(conn *sqlite.Conn, name string) (int32, error) {
	var v int32
	err := sqlitex.ExecuteTransient(conn, "PRAGMA "+name+";", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			v = stmt.ColumnInt32(0)
			return nil
		},
	})
	if err != nil {
		return 0, fmt.Errorf("reading PRAGMA %s: %w", name, err)
	}
	return v, nil
}

// pragmaBool reads a single-value boolean pragma.
func pragmaBool(conn *sqlite.Conn, name string) (bool, error) {
	v, err := pragmaInt32(conn, name)
	return v != 0, err
}

// pragmaText reads a single-value text pragma.
func pragmaText(conn *sqlite.Conn, name string) (string, error) {
	var v string
	err := sqlitex.ExecuteTransient(conn, "PRAGMA "+name+";", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			v = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("reading PRAGMA %s: %w", name, err)
	}
	return v, nil
}
