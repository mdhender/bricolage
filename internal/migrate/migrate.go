// Copyright (c) 2026 Michael D Henderson.

package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
)

// AppID is PRAGMA application_id for every database this system creates: the
// ASCII bytes of "CMS0" big-endian, 0x434D5330, 1129141040 decimal
// (DESIGN.md 13.2).
//
// It is a compile-time constant and it never changes. Changing it orphans
// every database in existence, because the check that reads it is the one
// thing standing between cmsd and a file belonging to some other program.
const AppID int32 = 0x434D5330

// AppIDString renders AppID the way it is written down: as hexadecimal and as
// the four characters it spells. Error messages an operator reads at three in
// the morning are more use with both.
func AppIDString(id int32) string {
	b := [4]byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("%#08x", uint32(id))
		}
	}
	return fmt.Sprintf("%#08x (%q)", uint32(id), string(b[:]))
}

//go:embed schema/*.sql
var schemaFS embed.FS

// fileName is the shape every migration file must have: a four-digit sequence
// number, an underscore, a lowercase name. The number is the order, and it is
// checked against the position rather than trusted, so a gap or a duplicate is
// a failure at startup rather than a migration silently skipped.
var fileName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// Migration is one embedded file: what it is called and what it does.
type Migration struct {
	// Name is the file name, which carries the sequence number.
	Name string

	// SQL is the file's contents, run inside one transaction by
	// sqlitemigration.
	SQL string
}

// Version is the PRAGMA user_version a database has once this migration has
// been applied. Migration N is version N; sqlitemigration counts from one.
func (m Migration) Version() int32 {
	n, _ := strconv.Atoi(m.Name[:4])
	return int32(n)
}

// all is the ordered migration list, read once at first use. A failure here is
// a programming error — the files are embedded in the binary — so it panics
// rather than making every caller carry an error that cannot happen in a
// shipped build.
var all = func() []Migration {
	entries, err := fs.ReadDir(schemaFS, "schema")
	if err != nil {
		panic(fmt.Sprintf("migrate: reading the embedded schema: %v", err))
	}

	// fs.ReadDir sorts by name, and the names begin with a zero-padded
	// sequence number, so the read order is the apply order.
	ms := make([]Migration, 0, len(entries))
	for i, e := range entries {
		if e.IsDir() {
			panic(fmt.Sprintf("migrate: schema/%s is a directory; migrations are flat", e.Name()))
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			panic(fmt.Sprintf("migrate: schema/%s does not match NNNN_name.sql", e.Name()))
		}
		if got, want := m[1], fmt.Sprintf("%04d", i+1); got != want {
			panic(fmt.Sprintf("migrate: schema/%s is numbered %s but is migration %s; the sequence has a gap or a duplicate", e.Name(), got, want))
		}
		b, err := fs.ReadFile(schemaFS, "schema/"+e.Name())
		if err != nil {
			panic(fmt.Sprintf("migrate: reading schema/%s: %v", e.Name(), err))
		}
		ms = append(ms, Migration{Name: e.Name(), SQL: string(b)})
	}
	if len(ms) == 0 {
		panic("migrate: no migrations are embedded")
	}
	return ms
}()

// All returns the embedded migrations in apply order.
func All() []Migration { return append([]Migration(nil), all...) }

// Count is the number of embedded migrations, and therefore the
// PRAGMA user_version a fully migrated database carries. cmsd requires the two
// to be equal, exactly (invariant 21).
func Count() int { return len(all) }

// Names returns the migration file names in apply order.
func Names() []string {
	out := make([]string, len(all))
	for i, m := range all {
		out[i] = m.Name
	}
	return out
}

// Schema returns the sqlitemigration schema for the first n migrations.
//
// n is a version, not an index: Schema(Count()) is everything, Schema(0) is
// the application ID and nothing else. A negative n means everything, so that
// a "--to" flag left unset is the whole schema.
//
// This is how "migrate up --to N" is expressed. sqlitemigration applies every
// migration in the schema it is handed and stops, so handing it a prefix is
// the entire implementation of a partial migration — there is no second code
// path that could disagree with the first about what migration 3 is.
func Schema(n int) (sqlitemigration.Schema, error) {
	if n < 0 {
		n = len(all)
	}
	if n > len(all) {
		return sqlitemigration.Schema{}, fmt.Errorf("migration %d: this binary embeds %d", n, len(all))
	}
	scripts := make([]string, n)
	for i := range n {
		scripts[i] = all[i].SQL
	}
	return sqlitemigration.Schema{AppID: AppID, Migrations: scripts}, nil
}

// Apply runs every migration up to version n against conn, stamping the
// application ID and advancing PRAGMA user_version once per migration, inside
// that migration's transaction (DESIGN.md 13.2). A negative n applies all of
// them.
//
// This is the only place migrations are applied. cmsd never calls it —
// cmsd opens and verifies, and repairing a version mismatch is the operator's
// decision, not a side effect of a restart (DESIGN.md 13.4).
func Apply(ctx context.Context, conn *sqlite.Conn, n int) error {
	schema, err := Schema(n)
	if err != nil {
		return err
	}
	if err := sqlitemigration.Migrate(ctx, conn, schema); err != nil {
		return err
	}
	return nil
}

// Pending returns the migrations a database at user_version v has not yet had
// applied, and the ones it has.
func Pending(v int32) (applied, pending []Migration) {
	for _, m := range all {
		if m.Version() <= v {
			applied = append(applied, m)
			continue
		}
		pending = append(pending, m)
	}
	return applied, pending
}

// Describe renders the migration list for "cmsdb migrate status".
func Describe(v int32) string {
	applied, pending := Pending(v)
	var b strings.Builder
	for _, m := range applied {
		fmt.Fprintf(&b, "  applied  %s\n", m.Name)
	}
	for _, m := range pending {
		fmt.Fprintf(&b, "  pending  %s\n", m.Name)
	}
	return b.String()
}
