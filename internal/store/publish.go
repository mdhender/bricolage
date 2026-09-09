// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Publishing's SQL (DESIGN.md 8.3, PLAN.md M9). All of it is here
// (invariant 2); internal/publish decides what to render and what to write,
// and this decides nothing.
//
// The shape worth reading before the statements is Publish's. One transaction
// does the whole of a publish: it deletes the resource rows for addresses this
// publish no longer produces, inserts or replaces the rows for the ones it
// does, moves documents.live_version_id, records the event, schedules the
// expiry jobs -- and then calls back into the caller to write the files, with
// the transaction still open.
//
// The callback is last and it is inside on purpose, and it is the answer to
// two acceptance criteria at once. A URI already taken by another document
// fails on UNIQUE (output_channel_id, uri) before a single byte is written, so
// the collision is detected by result code and the other document's file is
// never clobbered (PLAN.md M9 acceptance 4). A write that fails halfway rolls
// the whole transaction back, so the rows describe exactly the files that
// exist and a failure leaves neither partial output nor an orphaned row
// (acceptance 6). It is the same shape ApplyTransition uses for the engine's
// check: the store owns the transaction, the caller owns the decision, and
// neither can run without the other.

// resourceColumns is the projection every resource read shares. The document's
// uid and the channel's name are joined in so that the API can speak both
// without a second query (invariant 10).
const resourceColumns = `r.id AS id, r.document_id AS document_id,
	r.output_channel_id AS output_channel_id, r.version_id AS version_id,
	r.uri AS uri, r.path AS path, r.checksum AS checksum, r.bytes AS bytes,
	r.published_at AS published_at,
	d.uid AS document_uid, oc.name AS channel_name`

const resourceFrom = `
	  FROM published_resources r
	  JOIN documents d       ON d.id  = r.document_id
	  JOIN output_channels oc ON oc.id = r.output_channel_id`

// PublishRequest is one document's publish, in one or more output channels.
type PublishRequest struct {
	DocumentID int64

	// VersionID is the version that was rendered. It becomes
	// documents.live_version_id on success and is recorded on every resource
	// row (invariant 8).
	VersionID int64

	// ChannelIDs are the channels this publish covers. The expiry diff is
	// performed within these channels and no others: a publish to the web
	// channel must not expire what the print channel wrote.
	ChannelIDs []int64

	// Resources are the files to record, already rendered. Path and Checksum
	// describe bytes the callback is about to write.
	Resources []domain.Resource

	Now time.Time

	// Expire builds the job that deletes one address this publish no longer
	// produces. It is a function rather than a list because the rows to expire
	// are discovered inside the transaction, by the DELETE that removes them,
	// and a caller that had to know them first would have to read them in a
	// separate statement that the DELETE could then disagree with.
	//
	// The uid is minted by the caller for the reason every uid is: a ULID
	// encodes an instant and this package has no clock (invariant 3).
	Expire func(r domain.Resource) (NewJob, error)

	// Write writes the files. It is called last, inside the transaction, and
	// an error from it rolls everything back.
	Write func() error

	// Event is recorded in the same transaction (invariant 7). Its subject is
	// filled in here.
	Event domain.Event
}

// PublishResult is what one publish did.
type PublishResult struct {
	// Written are the resource rows as they now stand.
	Written []domain.Resource

	// Expired are the addresses this publish stopped producing, with the
	// paths whose files the scheduled expire jobs will delete.
	Expired []domain.Resource

	// Jobs are the expire jobs that were enqueued.
	Jobs []domain.Job
}

// Publish records one publish and writes its files, in one transaction.
func (db *DB) Publish(ctx context.Context, req PublishRequest) (PublishResult, error) {
	if req.DocumentID <= 0 || req.VersionID <= 0 {
		return PublishResult{}, fmt.Errorf(
			"publishing document %d: a publish names a document and a version: %w",
			req.DocumentID, domain.ErrInvalid)
	}
	if len(req.ChannelIDs) == 0 {
		return PublishResult{}, fmt.Errorf(
			"publishing document %d: no output channel: %w", req.DocumentID, domain.ErrInvalid)
	}
	if req.Write == nil {
		return PublishResult{}, fmt.Errorf("publishing document %d: nothing to write the files", req.DocumentID)
	}

	var out PublishResult
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		produced := make(map[int64][]string, len(req.ChannelIDs))
		for _, r := range req.Resources {
			produced[r.OutputChannelID] = append(produced[r.OutputChannelID], r.URI)
		}

		// The stale-expiry diff, one channel at a time. DESIGN.md 8.3 writes
		// it as a single DELETE with json_each over the new URIs; it is a
		// statement per channel here because the set of addresses a publish
		// produces is per channel and a single statement would need a
		// two-column IN over a JSON document to say so.
		for _, channelID := range req.ChannelIDs {
			expired, err := deleteStaleResources(conn, req.DocumentID, channelID, produced[channelID])
			if err != nil {
				return err
			}
			out.Expired = append(out.Expired, expired...)
		}

		// Insert or replace. A republish of the same address is the same row
		// with new bytes, which is why this is an upsert rather than a delete
		// and an insert: the row's identity is the address, and a row that
		// briefly did not exist is a window in which "cmsdb check" would call
		// the file on disk an orphan.
		for _, r := range req.Resources {
			if err := upsertResource(conn, req.DocumentID, req.VersionID, r, req.Now); err != nil {
				return err
			}
		}

		// live_version_id is set here and nowhere else, so it moves if and
		// only if the whole publish commits (PLAN.md M9 acceptance 5).
		if err := setLiveVersion(conn, req.DocumentID, req.VersionID, req.Now); err != nil {
			return err
		}

		req.Event.SubjectKind = domain.SubjectDocument
		req.Event.SubjectID = req.DocumentID
		if _, err := recordEvent(conn, req.Event); err != nil {
			return err
		}

		if req.Expire != nil {
			for _, r := range out.Expired {
				n, err := req.Expire(r)
				if err != nil {
					return err
				}
				job, err := enqueueJob(conn, n)
				if err != nil {
					return err
				}
				out.Jobs = append(out.Jobs, job)
			}
		}

		// Last, and inside. Everything above has already refused a collision;
		// anything this reports rolls the rows back with it.
		if err := req.Write(); err != nil {
			return err
		}

		var err error
		out.Written, err = resourcesForDocument(conn, req.DocumentID)
		return err
	})
	if err != nil {
		return PublishResult{}, err
	}
	return out, nil
}

// deleteStaleResources removes the rows for addresses this publish no longer
// produces, returning what it removed (DESIGN.md 8.3).
//
// The RETURNING clause is the whole reason this is one statement: the rows
// have to be read to schedule their expiry and deleted so that nothing claims
// the file any more, and doing it as a SELECT then a DELETE is two statements
// that can disagree about which rows they were.
//
// json_each over a JSON array is how the new URI set reaches the statement,
// which is DESIGN.md 8.3's own form. A publish that produces nothing in a
// channel passes an empty array, and every row in that channel is expired --
// which is correct: a document that no longer builds an address there has no
// business having a file there.
func deleteStaleResources(conn *sqlite.Conn, documentID, channelID int64, keep []string) ([]domain.Resource, error) {
	uris, err := jsonArray(keep)
	if err != nil {
		return nil, err
	}

	var out []domain.Resource
	err = run(conn, fmt.Sprintf("expiring the stale resources of document %d", documentID), `
		DELETE FROM published_resources
		 WHERE document_id = :document AND output_channel_id = :channel
		   AND uri NOT IN (SELECT value FROM json_each(:uris))
		RETURNING id, document_id, output_channel_id, version_id,
		          uri, path, checksum, bytes, published_at`,
		func(stmt *sqlite.Stmt) {
			stmt.SetInt64(":document", documentID)
			stmt.SetInt64(":channel", channelID)
			stmt.SetText(":uris", uris)
		},
		func(stmt *sqlite.Stmt) error {
			r, err := scanDeletedResource(stmt)
			if err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	return out, err
}

// upsertResource records one written file.
//
// The conflict target is the unique index, so a republish of the same address
// updates the row in place and a different document's claim on that address is
// a constraint violation rather than a silent takeover. WHERE excluded on the
// update is deliberately absent: the same document republishing the same
// address to a new version is exactly the case, and refusing it would refuse
// every ordinary republish.
func upsertResource(conn *sqlite.Conn, documentID, versionID int64, r domain.Resource, now time.Time) error {
	what := fmt.Sprintf("recording %s for document %d", r.URI, documentID)
	err := run(conn, what, `
		INSERT INTO published_resources
		       (document_id, output_channel_id, version_id, uri, path, checksum, bytes, published_at)
		VALUES (:document, :channel, :version, :uri, :path, :checksum, :bytes, :now)
		ON CONFLICT (output_channel_id, uri) DO UPDATE
		   SET document_id  = excluded.document_id,
		       version_id   = excluded.version_id,
		       path         = excluded.path,
		       checksum     = excluded.checksum,
		       bytes        = excluded.bytes,
		       published_at = excluded.published_at
		 WHERE published_resources.document_id = excluded.document_id`,
		func(stmt *sqlite.Stmt) {
			stmt.SetInt64(":document", documentID)
			stmt.SetInt64(":channel", r.OutputChannelID)
			stmt.SetInt64(":version", versionID)
			stmt.SetText(":uri", r.URI)
			stmt.SetText(":path", r.Path)
			stmt.SetText(":checksum", r.Checksum)
			stmt.SetInt64(":bytes", r.Bytes)
			stmt.SetText(":now", formatTime(now))
		}, nil)
	if err != nil {
		return err
	}
	if conn.Changes() == 1 {
		return nil
	}

	// The WHERE on the DO UPDATE refused: the address belongs to another
	// document. Name it, because "URI already in use" without the other
	// document is a message that sends somebody grepping
	// (PLAN.md M9 acceptance 4).
	owner, err := resourceByURI(conn, r.OutputChannelID, r.URI)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s is already in use by document %s: %w", r.URI, owner.DocumentUID, domain.ErrConflict)
}

// setLiveVersion moves documents.live_version_id.
//
// It is a separate statement from anything in workflow.go and it writes no
// state: what is live and where a document has got to in its process are two
// different facts, and a document can sit in draft being revised while the
// previously published version continues to serve (0005's header note).
func setLiveVersion(conn *sqlite.Conn, documentID, versionID int64, now time.Time) error {
	err := run(conn, fmt.Sprintf("marking version %d of document %d live", versionID, documentID), `
		UPDATE documents
		   SET live_version_id = :version,
		       updated_at = :now
		 WHERE id = :id`,
		func(stmt *sqlite.Stmt) {
			stmt.SetInt64(":version", versionID)
			stmt.SetText(":now", formatTime(now))
			stmt.SetInt64(":id", documentID)
		}, nil)
	if err != nil {
		return err
	}
	if conn.Changes() != 1 {
		return notFound(fmt.Sprintf("document %d", documentID))
	}
	return nil
}

// ResourcesForDocument returns every file recorded for a document, in address
// order within channel.
func (db *DB) ResourcesForDocument(ctx context.Context, documentID int64) ([]domain.Resource, error) {
	var out []domain.Resource
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		out, err = resourcesForDocument(conn, documentID)
		return err
	})
	return out, err
}

func resourcesForDocument(conn *sqlite.Conn, documentID int64) ([]domain.Resource, error) {
	var out []domain.Resource
	err := run(conn, fmt.Sprintf("the published resources of document %d", documentID),
		`SELECT `+resourceColumns+resourceFrom+`
		 WHERE r.document_id = :document
		 ORDER BY oc.name, r.uri`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":document", documentID) },
		func(stmt *sqlite.Stmt) error {
			r, err := scanResource(stmt)
			if err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	return out, err
}

// resourceByURI reads the resource at one address in one channel.
//
// It is unexported because it has one caller and one purpose: naming the
// document that already holds an address, in the message a collision produces.
// Nothing else asks the question -- a client asks what a document's addresses
// are, not who holds one -- and an exported reader nobody called would be a
// name that lies about what this package is for.
func resourceByURI(conn *sqlite.Conn, channelID int64, uri string) (domain.Resource, error) {
	var out domain.Resource
	err := one(conn, fmt.Sprintf("the resource at %s", uri),
		`SELECT `+resourceColumns+resourceFrom+`
		 WHERE r.output_channel_id = :channel AND r.uri = :uri`,
		func(stmt *sqlite.Stmt) {
			stmt.SetInt64(":channel", channelID)
			stmt.SetText(":uri", uri)
		},
		func(stmt *sqlite.Stmt) error {
			var err error
			out, err = scanResource(stmt)
			return err
		})
	return out, err
}

// AllResources returns every recorded resource, in path order.
//
// It is what "cmsdb check" reconciles the output tree against
// (PLAN.md M9 acceptance 7). Path order because the walk it is compared with
// produces paths in that order, and comparing two sorted lists is the whole of
// the reconciliation.
func (db *DB) AllResources(ctx context.Context) ([]domain.Resource, error) {
	var out []domain.Resource
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "every published resource",
			`SELECT `+resourceColumns+resourceFrom+` ORDER BY r.path`, nil,
			func(stmt *sqlite.Stmt) error {
				r, err := scanResource(stmt)
				if err != nil {
					return err
				}
				out = append(out, r)
				return nil
			})
	})
	return out, err
}

// There is deliberately no DeleteResource.
//
// Expiry does not need one: the rows it would delete are already gone, removed
// by the publish that stopped producing their addresses, in the transaction
// that scheduled the jobs. A method nothing calls is the rule column nothing
// reads (invariant 6), and the one case somebody might reach for it -- a file
// an operator deleted by hand -- is what "cmsdb check --output" reports rather
// than silently repairs.

// LatestCheckedInVersion returns the newest version of a document that has
// been checked in.
//
// It is what a publish pins. The document's current version may be an open
// working draft -- it is, for as long as somebody has the document checked out
// -- and publishing that would put an unfinished page live, which is the
// failure the whole check-in model exists to prevent.
func (db *DB) LatestCheckedInVersion(ctx context.Context, documentID int64) (domain.Version, error) {
	var out domain.Version
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return latestCheckedIn(conn, documentID, &out)
	})
	return out, err
}

// VersionByID reads one version by its primary key. The id comes from a job
// payload written by this system and never from the wire (invariant 10).
func (db *DB) VersionByID(ctx context.Context, id int64) (domain.Version, error) {
	var out domain.Version
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return versionByID(conn, id, &out)
	})
	return out, err
}

// DocumentByID reads one document by its primary key, for the same reason.
func (db *DB) DocumentByID(ctx context.Context, id int64) (domain.Document, error) {
	var out domain.Document
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return documentByID(conn, id, &out)
	})
	return out, err
}

// OutputChannelByID reads one output channel by its primary key.
func (db *DB) OutputChannelByID(ctx context.Context, id int64) (domain.OutputChannel, error) {
	var out domain.OutputChannel
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return channelByID(conn, id, &out)
	})
	return out, err
}

// jsonArray renders a list of strings as the JSON array json_each reads.
//
// An empty list is "[]" and never "null", which matters: json_each("null")
// yields one row holding NULL, and "uri NOT IN (NULL)" is NULL rather than
// true, so a publish that produced nothing in a channel would expire nothing
// there instead of everything.
func jsonArray(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encoding a URI list: %w", err)
	}
	return string(b), nil
}

// scanDeletedResource reads a row of the expiry DELETE's RETURNING clause,
// which carries the table's own columns and neither of the two joined ones: a
// row that has just been deleted is not being described to anybody by the name
// of its document.
func scanDeletedResource(stmt *sqlite.Stmt) (domain.Resource, error) {
	published, err := parseTime(stmt.GetText("published_at"))
	if err != nil {
		return domain.Resource{}, fmt.Errorf("resource %d: published_at: %w", stmt.GetInt64("id"), err)
	}
	return domain.Resource{
		ID:              stmt.GetInt64("id"),
		DocumentID:      stmt.GetInt64("document_id"),
		OutputChannelID: stmt.GetInt64("output_channel_id"),
		VersionID:       stmt.GetInt64("version_id"),
		URI:             stmt.GetText("uri"),
		Path:            stmt.GetText("path"),
		Checksum:        stmt.GetText("checksum"),
		Bytes:           stmt.GetInt64("bytes"),
		PublishedAt:     published,
	}, nil
}

func scanResource(stmt *sqlite.Stmt) (domain.Resource, error) {
	r, err := scanDeletedResource(stmt)
	if err != nil {
		return domain.Resource{}, err
	}
	r.DocumentUID = stmt.GetText("document_uid")
	r.ChannelName = stmt.GetText("channel_name")
	return r, nil
}
