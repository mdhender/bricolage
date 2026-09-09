// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Output channels: where content goes and what its address looks like
// (DESIGN.md 5.3, PLAN.md M7).
//
// There is no URI arithmetic in this file. The formats are stored and read
// back verbatim; expanding one is domain.BuildURI, which is pure and which a
// table of golden vectors covers.

// channelColumns is the projection every output channel read shares.
const channelColumns = `
	id, uid, site_id, name, protocol, filename, file_ext,
	uri_format, fixed_uri_format, use_slug, uri_case`

// CreateOutputChannel writes an output channel.
//
// A duplicate name on the same site is a *ConstraintError answering to
// domain.ErrConflict, which is what makes "cmsdb seed" idempotent without a
// read-then-write race.
func (db *DB) CreateOutputChannel(ctx context.Context, oc domain.OutputChannel, event domain.Event) (domain.OutputChannel, error) {
	if err := oc.Validate(); err != nil {
		return domain.OutputChannel{}, err
	}
	var out domain.OutputChannel
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "creating output channel "+oc.Name, `
			INSERT INTO output_channels
			       (uid, site_id, name, protocol, filename, file_ext,
			        uri_format, fixed_uri_format, use_slug, uri_case)
			VALUES (:uid, :site_id, :name, :protocol, :filename, :file_ext,
			        :uri_format, :fixed_uri_format, :use_slug, :uri_case)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", oc.UID)
				stmt.SetInt64(":site_id", oc.SiteID)
				bindChannel(stmt, oc)
			}, nil)
		if err != nil {
			return err
		}
		id := conn.LastInsertRowID()

		if event.Type != "" {
			event.SubjectKind = domain.SubjectOutputChannel
			event.SubjectID = id
			if _, err := recordEvent(conn, event); err != nil {
				return err
			}
		}
		return channelByID(conn, id, &out)
	})
	return out, err
}

// UpdateOutputChannel replaces an output channel's configuration.
//
// Everything but the site and the uid is replaceable, because everything but
// the site and the uid is a decision about how addresses look, and a URI
// format that could not be corrected would be a URI format nobody dared write.
// The site is not: moving a channel between sites would change the domain
// every address it has produced resolves against.
func (db *DB) UpdateOutputChannel(ctx context.Context, oc domain.OutputChannel, event domain.Event) (domain.OutputChannel, error) {
	if err := oc.Validate(); err != nil {
		return domain.OutputChannel{}, err
	}
	var out domain.OutputChannel
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "updating output channel "+oc.Name, `
			UPDATE output_channels
			   SET name = :name, protocol = :protocol, filename = :filename,
			       file_ext = :file_ext, uri_format = :uri_format,
			       fixed_uri_format = :fixed_uri_format, use_slug = :use_slug,
			       uri_case = :uri_case
			 WHERE id = :id`,
			func(stmt *sqlite.Stmt) {
				bindChannel(stmt, oc)
				stmt.SetInt64(":id", oc.ID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return notFound(fmt.Sprintf("output channel %d", oc.ID))
		}

		if event.Type != "" {
			event.SubjectKind = domain.SubjectOutputChannel
			event.SubjectID = oc.ID
			if _, err := recordEvent(conn, event); err != nil {
				return err
			}
		}
		return channelByID(conn, oc.ID, &out)
	})
	return out, err
}

// bindChannel binds the columns an insert and an update share. The uid and
// the site are not among them: an update changes neither, and moving a channel
// between sites would change the domain every address it has produced resolves
// against.
func bindChannel(stmt *sqlite.Stmt, oc domain.OutputChannel) {
	stmt.SetText(":name", oc.Name)
	stmt.SetText(":protocol", oc.Protocol)
	stmt.SetText(":filename", oc.Filename)
	stmt.SetText(":file_ext", oc.FileExt)
	stmt.SetText(":uri_format", oc.URIFormat)
	stmt.SetText(":fixed_uri_format", oc.FixedURIFormat)
	stmt.SetBool(":use_slug", oc.UseSlug)
	stmt.SetText(":uri_case", oc.URICase)
}

// OutputChannelByUID reads an output channel by the identifier the API speaks
// (invariant 10).
func (db *DB) OutputChannelByUID(ctx context.Context, uid string) (domain.OutputChannel, error) {
	var oc domain.OutputChannel
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("output channel %q", uid),
			`SELECT `+channelColumns+` FROM output_channels WHERE uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error { oc = scanChannel(stmt); return nil })
	})
	return oc, err
}

func channelByID(conn *sqlite.Conn, id int64, oc *domain.OutputChannel) error {
	return one(conn, fmt.Sprintf("output channel %d", id),
		`SELECT `+channelColumns+` FROM output_channels WHERE id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error { *oc = scanChannel(stmt); return nil })
}

// OutputChannelByName reads one site's channel by name, which is how "cmsdb
// seed" checks before creating one.
func (db *DB) OutputChannelByName(ctx context.Context, siteID int64, name string) (domain.OutputChannel, error) {
	var oc domain.OutputChannel
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("output channel %q on site %d", name, siteID),
			`SELECT `+channelColumns+` FROM output_channels WHERE site_id = :site_id AND name = :name`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":site_id", siteID)
				stmt.SetText(":name", name)
			},
			func(stmt *sqlite.Stmt) error { oc = scanChannel(stmt); return nil })
	})
	return oc, err
}

// ListOutputChannels returns one site's channels in name order, or every
// site's when siteID is 0.
func (db *DB) ListOutputChannels(ctx context.Context, siteID int64) ([]domain.OutputChannel, error) {
	var out []domain.OutputChannel
	query := `SELECT ` + channelColumns + ` FROM output_channels ORDER BY site_id, name`
	bind := func(*sqlite.Stmt) {}
	if siteID != 0 {
		query = `SELECT ` + channelColumns + ` FROM output_channels WHERE site_id = :site_id ORDER BY name`
		bind = func(stmt *sqlite.Stmt) { stmt.SetInt64(":site_id", siteID) }
	}
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing output channels", query, bind, func(stmt *sqlite.Stmt) error {
			out = append(out, scanChannel(stmt))
			return nil
		})
	})
	return out, err
}

func scanChannel(stmt *sqlite.Stmt) domain.OutputChannel {
	return domain.OutputChannel{
		ID:             stmt.GetInt64("id"),
		UID:            stmt.GetText("uid"),
		SiteID:         stmt.GetInt64("site_id"),
		Name:           stmt.GetText("name"),
		Protocol:       stmt.GetText("protocol"),
		Filename:       stmt.GetText("filename"),
		FileExt:        stmt.GetText("file_ext"),
		URIFormat:      stmt.GetText("uri_format"),
		FixedURIFormat: stmt.GetText("fixed_uri_format"),
		UseSlug:        stmt.GetBool("use_slug"),
		URICase:        stmt.GetText("uri_case"),
	}
}

// SiteByID reads one site. It is here because an absolute URL needs the
// domain, and a transport does not query (DESIGN.md 3).
func (db *DB) SiteByID(ctx context.Context, id int64) (domain.Site, error) {
	var s domain.Site
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("site %d", id),
			`SELECT id, uid, name, domain, active FROM sites WHERE id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
			func(stmt *sqlite.Stmt) error { s = scanSite(stmt); return nil })
	})
	return s, err
}

// ListSites returns every site in domain order.
func (db *DB) ListSites(ctx context.Context) ([]domain.Site, error) {
	var out []domain.Site
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing sites",
			`SELECT id, uid, name, domain, active FROM sites ORDER BY domain`, nil,
			func(stmt *sqlite.Stmt) error {
				out = append(out, scanSite(stmt))
				return nil
			})
	})
	return out, err
}

func scanSite(stmt *sqlite.Stmt) domain.Site {
	return domain.Site{
		ID:     stmt.GetInt64("id"),
		UID:    stmt.GetText("uid"),
		Name:   stmt.GetText("name"),
		Domain: stmt.GetText("domain"),
		Active: stmt.GetBool("active"),
	}
}
