// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Documents, versions, and element types. All the SQL for M3 is here
// (invariant 2); internal/service decides what may happen and this decides
// nothing.
//
// Every method that changes more than one row runs inside db.Tx, so the change
// and the event recording it commit or roll back together (invariant 7). The
// methods that take a *sqlite.Conn rather than a context are the ones a
// service composes into a larger transaction.

// documentColumns is the projection every document read shares. The element
// type's key name is joined in because the API speaks it and the integer key
// never leaves the process (invariant 10).
const documentColumns = `
	d.id AS id, d.uid AS uid, d.site_id AS site_id, d.kind AS kind,
	d.element_type_id AS element_type_id, et.key_name AS element_type_key,
	et.fixed_uri AS element_type_fixed_uri,
	d.workflow_id AS workflow_id, d.state AS state,
	cat.id AS category_id, cat.path AS category_path,
	d.assigned_to AS assigned_to, d.due_at AS due_at,
	d.locked_by AS locked_by, d.lock_expires_at AS lock_expires_at,
	d.current_version_id AS current_version_id, d.live_version_id AS live_version_id,
	d.created_at AS created_at, d.updated_at AS updated_at`

// documentFrom is the FROM clause every document read shares.
//
// The element type is joined because the API speaks its key name and the
// integer key never leaves the process (invariant 10), and because
// domain.BuildURI needs its fixed_uri flag and is pure, so it cannot go and
// look. The primary category is joined for the same reason twice over: the
// authorization resolver matches a category-scoped grant by prefix on the
// path and performs no I/O (DESIGN.md 7.2), and %{categories} expands from it.
//
// Both category joins are LEFT: a document filed nowhere is a document, its
// category path is empty, and a grant that names a category does not match it.
const documentFrom = `
	  FROM documents d
	  JOIN element_types et ON et.id = d.element_type_id
	  LEFT JOIN document_categories dc ON dc.document_id = d.id AND dc.primary_cat = 1
	  LEFT JOIN categories cat ON cat.id = dc.category_id`

// versionColumns is the projection every version read shares.
const versionColumns = `
	id, document_id, version, title, slug, cover_date, content, note,
	created_by, created_at, checked_in_at`

// NewElementType is what CreateElementType is given.
type NewElementType struct {
	UID       string
	KeyName   string
	Name      string
	Kind      string
	TopLevel  bool
	FixedURI  bool
	Paginated bool

	// Schema is the JSON field definition document. M3 stored it and did not
	// validate content against it; M7 validates a check-in against it
	// (domain.ValidateContent, PLAN.md M7 acceptance 5).
	Schema    string
	CreatedAt time.Time

	// Event is recorded in the same transaction as the insert, when there is
	// one. "cmsdb seed" writes an element type before there is anybody to
	// have written it and passes none; the API always passes one.
	Event domain.Event
}

// CreateElementType inserts an element type. A duplicate key name is a
// *ConstraintError answering to domain.ErrConflict, which is what makes
// "cmsdb seed" idempotent without a read-then-write race.
func (db *DB) CreateElementType(ctx context.Context, et NewElementType) (domain.ElementType, error) {
	var out domain.ElementType
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "creating element type "+et.KeyName, `
			INSERT INTO element_types
			       (uid, key_name, name, kind, top_level, fixed_uri, paginated, schema, created_at)
			VALUES (:uid, :key_name, :name, :kind, :top_level, :fixed_uri, :paginated, :schema, :created_at)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", et.UID)
				stmt.SetText(":key_name", et.KeyName)
				stmt.SetText(":name", et.Name)
				stmt.SetText(":kind", et.Kind)
				stmt.SetBool(":top_level", et.TopLevel)
				stmt.SetBool(":fixed_uri", et.FixedURI)
				stmt.SetBool(":paginated", et.Paginated)
				stmt.SetText(":schema", domain.NormalizeContent(et.Schema))
				stmt.SetText(":created_at", formatTime(et.CreatedAt))
			}, nil)
		if err != nil {
			return err
		}
		id := conn.LastInsertRowID()
		if et.Event.Type != "" {
			et.Event.SubjectKind = domain.SubjectElementType
			et.Event.SubjectID = id
			if _, err := recordEvent(conn, et.Event); err != nil {
				return err
			}
		}
		out = domain.ElementType{
			ID:        id,
			UID:       et.UID,
			KeyName:   et.KeyName,
			Name:      et.Name,
			Kind:      et.Kind,
			TopLevel:  et.TopLevel,
			FixedURI:  et.FixedURI,
			Paginated: et.Paginated,
			Schema:    domain.NormalizeContent(et.Schema),
			CreatedAt: et.CreatedAt.UTC(),
		}
		return nil
	})
	return out, err
}

const elementTypeColumns = `id, uid, key_name, name, kind, top_level, fixed_uri, paginated, schema, created_at`

// UpdateElementType replaces an element type's mutable configuration.
//
// The key name and the kind are not among them. The key name is what a client
// names the type by and what a document's response carries (invariant 10);
// the kind decides which documents may use it and is a grant scope dimension,
// so changing it would silently change who may touch every document of that
// type. Both are decided at creation, like a document's kind.
//
// The schema is the interesting one, and it is replaceable on purpose: a
// schema that could not be corrected would be a schema nobody dared write.
// Content already checked in is not revalidated -- a version is immutable and
// what it was valid against is what it was checked in against -- so a
// tightened schema takes effect at the next check-in, which is where a person
// can do something about it.
func (db *DB) UpdateElementType(ctx context.Context, et domain.ElementType, event domain.Event) (domain.ElementType, error) {
	var out domain.ElementType
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "updating element type "+et.KeyName, `
			UPDATE element_types
			   SET name = :name, top_level = :top_level, fixed_uri = :fixed_uri,
			       paginated = :paginated, schema = :schema
			 WHERE id = :id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":name", et.Name)
				stmt.SetBool(":top_level", et.TopLevel)
				stmt.SetBool(":fixed_uri", et.FixedURI)
				stmt.SetBool(":paginated", et.Paginated)
				stmt.SetText(":schema", domain.NormalizeContent(et.Schema))
				stmt.SetInt64(":id", et.ID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return notFound(fmt.Sprintf("element type %d", et.ID))
		}

		if event.Type != "" {
			event.SubjectKind = domain.SubjectElementType
			event.SubjectID = et.ID
			if _, err := recordEvent(conn, event); err != nil {
				return err
			}
		}
		return one(conn, "element type "+et.KeyName,
			`SELECT `+elementTypeColumns+` FROM element_types WHERE id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", et.ID) },
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanElementType(stmt)
				return err
			})
	})
	return out, err
}

// ElementTypeByUID reads an element type by the identifier the API speaks for
// everything else. The key name is the one a client normally uses, because it
// is what a document names its type by; this is here so that an administrative
// update can address a row whose key name is being read off a listing.
func (db *DB) ElementTypeByUID(ctx context.Context, uid string) (domain.ElementType, error) {
	var et domain.ElementType
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("element type %q", uid),
			`SELECT `+elementTypeColumns+` FROM element_types WHERE uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error {
				var err error
				et, err = scanElementType(stmt)
				return err
			})
	})
	return et, err
}

// ElementTypeByKeyName reads an element type by its external identifier.
func (db *DB) ElementTypeByKeyName(ctx context.Context, keyName string) (domain.ElementType, error) {
	var et domain.ElementType
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("element type %q", keyName),
			`SELECT `+elementTypeColumns+` FROM element_types WHERE key_name = :key_name`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":key_name", keyName) },
			func(stmt *sqlite.Stmt) error {
				var err error
				et, err = scanElementType(stmt)
				return err
			})
	})
	return et, err
}

// ListElementTypes returns every element type, in key name order so that the
// list is stable in output and in tests.
func (db *DB) ListElementTypes(ctx context.Context) ([]domain.ElementType, error) {
	var out []domain.ElementType
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing element types",
			`SELECT `+elementTypeColumns+` FROM element_types ORDER BY key_name`, nil,
			func(stmt *sqlite.Stmt) error {
				et, err := scanElementType(stmt)
				if err != nil {
					return err
				}
				out = append(out, et)
				return nil
			})
	})
	return out, err
}

func scanElementType(stmt *sqlite.Stmt) (domain.ElementType, error) {
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.ElementType{}, fmt.Errorf("element type %d: created_at: %w", stmt.GetInt64("id"), err)
	}
	return domain.ElementType{
		ID:        stmt.GetInt64("id"),
		UID:       stmt.GetText("uid"),
		KeyName:   stmt.GetText("key_name"),
		Name:      stmt.GetText("name"),
		Kind:      stmt.GetText("kind"),
		TopLevel:  stmt.GetBool("top_level"),
		FixedURI:  stmt.GetBool("fixed_uri"),
		Paginated: stmt.GetBool("paginated"),
		Schema:    stmt.GetText("schema"),
		CreatedAt: created,
	}, nil
}

// NewDocument is what CreateDocument is given: the document's identity and the
// first working draft, which are written together because a document with no
// version has no title and nothing to show.
type NewDocument struct {
	UID           string
	SiteID        int64
	Kind          string
	ElementTypeID int64

	// WorkflowID and State place the document in an editorial process. They
	// are supplied by the caller rather than defaulted here, because the
	// workflow that governs a document is a decision -- which workflow, on
	// which site, for which kind -- and this package decides nothing.
	//
	// State is the workflow's initial state and nothing else: creating a
	// document is not a transition, and internal/workflow remains the only
	// thing that moves one (invariant 4). The composite foreign key refuses
	// a state the workflow does not declare, whatever is passed.
	WorkflowID int64
	State      string

	Title     string
	Slug      string
	CoverDate string
	Content   string

	CreatedBy int64
	CreatedAt time.Time

	// Event is recorded in the same transaction as the insert. Its SubjectID
	// is filled in here, because the document does not have an id until the
	// INSERT has run.
	Event domain.Event
}

// CreateDocument writes a document and its first working draft, and records
// the event, in one transaction.
//
// The pointer dance is the price of the cycle between the two tables: the
// document is inserted with current_version_id NULL, the version is written
// against the document's id, and the pointer is set. SQLite checks foreign
// keys per statement, so all three see a consistent database.
func (db *DB) CreateDocument(ctx context.Context, n NewDocument) (domain.Document, domain.Version, error) {
	var (
		doc domain.Document
		ver domain.Version
	)
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "creating document "+n.UID, `
			INSERT INTO documents (uid, site_id, kind, element_type_id, workflow_id, state, created_at, updated_at)
			VALUES (:uid, :site_id, :kind, :element_type_id, :workflow_id, :state, :created_at, :updated_at)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", n.UID)
				stmt.SetInt64(":site_id", n.SiteID)
				stmt.SetText(":kind", n.Kind)
				stmt.SetInt64(":element_type_id", n.ElementTypeID)
				stmt.SetInt64(":workflow_id", n.WorkflowID)
				stmt.SetText(":state", n.State)
				stmt.SetText(":created_at", formatTime(n.CreatedAt))
				stmt.SetText(":updated_at", formatTime(n.CreatedAt))
			}, nil)
		if err != nil {
			return err
		}
		documentID := conn.LastInsertRowID()

		ver, err = insertVersion(conn, domain.Version{
			DocumentID: documentID,
			Number:     1,
			Title:      n.Title,
			Slug:       n.Slug,
			CoverDate:  n.CoverDate,
			Content:    domain.NormalizeContent(n.Content),
			CreatedBy:  n.CreatedBy,
			CreatedAt:  n.CreatedAt,
		})
		if err != nil {
			return err
		}
		if err := setCurrentVersion(conn, documentID, ver.ID, n.CreatedAt); err != nil {
			return err
		}

		n.Event.SubjectKind = domain.SubjectDocument
		n.Event.SubjectID = documentID
		if _, err := recordEvent(conn, n.Event); err != nil {
			return err
		}
		return documentByID(conn, documentID, &doc)
	})
	return doc, ver, err
}

// insertVersion writes one version row on a connection the caller holds.
func insertVersion(conn *sqlite.Conn, v domain.Version) (domain.Version, error) {
	err := run(conn, fmt.Sprintf("writing version %d of document %d", v.Number, v.DocumentID), `
		INSERT INTO document_versions
		       (document_id, version, title, slug, cover_date, content, note, created_by, created_at, checked_in_at)
		VALUES (:document_id, :version, :title, :slug, :cover_date, :content, :note, :created_by, :created_at, :checked_in_at)`,
		func(stmt *sqlite.Stmt) {
			stmt.SetInt64(":document_id", v.DocumentID)
			stmt.SetInt64(":version", int64(v.Number))
			stmt.SetText(":title", v.Title)
			stmt.SetText(":slug", v.Slug)
			bindNullTime(stmt, ":cover_date", v.CoverDate)
			stmt.SetText(":content", domain.NormalizeContent(v.Content))
			bindNullTime(stmt, ":note", v.Note)
			stmt.SetInt64(":created_by", v.CreatedBy)
			stmt.SetText(":created_at", formatTime(v.CreatedAt))
			if v.CheckedInAt.IsZero() {
				stmt.SetNull(":checked_in_at")
			} else {
				stmt.SetText(":checked_in_at", formatTime(v.CheckedInAt))
			}
		}, nil)
	if err != nil {
		return domain.Version{}, err
	}
	v.ID = conn.LastInsertRowID()
	v.CreatedAt = v.CreatedAt.UTC()
	v.Content = domain.NormalizeContent(v.Content)
	return v, nil
}

// bindNullTime binds a text column whose empty value is NULL rather than "".
// cover_date and note are both "absent or a value", and an empty string in
// either is a value nobody meant to store.
func bindNullTime(stmt *sqlite.Stmt, param, v string) {
	if v == "" {
		stmt.SetNull(param)
		return
	}
	stmt.SetText(param, v)
}

func setCurrentVersion(conn *sqlite.Conn, documentID, versionID int64, now time.Time) error {
	return run(conn, fmt.Sprintf("pointing document %d at version %d", documentID, versionID),
		`UPDATE documents SET current_version_id = :version_id, updated_at = :now WHERE id = :id`,
		func(stmt *sqlite.Stmt) {
			if versionID == 0 {
				stmt.SetNull(":version_id")
			} else {
				stmt.SetInt64(":version_id", versionID)
			}
			stmt.SetText(":now", formatTime(now))
			stmt.SetInt64(":id", documentID)
		}, nil)
}

// DocumentByUID reads a document by the identifier the API speaks
// (invariant 10).
func (db *DB) DocumentByUID(ctx context.Context, uid string) (domain.Document, error) {
	var d domain.Document
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return documentByUID(conn, uid, &d)
	})
	return d, err
}

func documentByUID(conn *sqlite.Conn, uid string, d *domain.Document) error {
	return one(conn, fmt.Sprintf("document %q", uid), `
		SELECT `+documentColumns+documentFrom+`
		 WHERE d.uid = :uid`,
		func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*d, err = scanDocument(stmt)
			return err
		})
}

func documentByID(conn *sqlite.Conn, id int64, d *domain.Document) error {
	return one(conn, fmt.Sprintf("document %d", id), `
		SELECT `+documentColumns+documentFrom+`
		 WHERE d.id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*d, err = scanDocument(stmt)
			return err
		})
}

// Listing documents is QueryDocuments, in queue.go. M3 had a ListDocuments
// here that took a limit and nothing else; M5 gave it the filters that make it
// a queue query, and keeping the unfiltered one alongside would have been two
// list paths that answer the same question differently. An unfiltered
// domain.DocumentFilter is the M3 list.

func scanDocument(stmt *sqlite.Stmt) (domain.Document, error) {
	id := stmt.GetInt64("id")
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Document{}, fmt.Errorf("document %d: created_at: %w", id, err)
	}
	updated, err := parseTime(stmt.GetText("updated_at"))
	if err != nil {
		return domain.Document{}, fmt.Errorf("document %d: updated_at: %w", id, err)
	}
	var due, lockExpires time.Time
	if s := nullText(stmt, "due_at"); s != nil {
		if due, err = parseTime(*s); err != nil {
			return domain.Document{}, fmt.Errorf("document %d: due_at: %w", id, err)
		}
	}
	if s := nullText(stmt, "lock_expires_at"); s != nil {
		if lockExpires, err = parseTime(*s); err != nil {
			return domain.Document{}, fmt.Errorf("document %d: lock_expires_at: %w", id, err)
		}
	}

	d := domain.Document{
		ID:                  id,
		UID:                 stmt.GetText("uid"),
		SiteID:              stmt.GetInt64("site_id"),
		Kind:                stmt.GetText("kind"),
		ElementTypeID:       stmt.GetInt64("element_type_id"),
		ElementTypeKey:      stmt.GetText("element_type_key"),
		ElementTypeFixedURI: stmt.GetBool("element_type_fixed_uri"),
		WorkflowID:          stmt.GetInt64("workflow_id"),
		State:               stmt.GetText("state"),
		DueAt:               due,
		Lock:                domain.Lock{ExpiresAt: lockExpires},
		CreatedAt:           created,
		UpdatedAt:           updated,
	}
	if v := nullInt64(stmt, "category_id"); v != nil {
		d.CategoryID = *v
	}
	if s := nullText(stmt, "category_path"); s != nil {
		d.CategoryPath = *s
	}
	if v := nullInt64(stmt, "assigned_to"); v != nil {
		d.AssignedTo = *v
	}
	if v := nullInt64(stmt, "locked_by"); v != nil {
		d.Lock.UserID = *v
	}
	if v := nullInt64(stmt, "current_version_id"); v != nil {
		d.CurrentVersionID = *v
	}
	if v := nullInt64(stmt, "live_version_id"); v != nil {
		d.LiveVersionID = *v
	}
	return d, nil
}

// VersionsForDocument returns a document's versions, newest first. The open
// working draft, when there is one, is the first row.
func (db *DB) VersionsForDocument(ctx context.Context, documentID int64) ([]domain.Version, error) {
	var out []domain.Version
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("versions of document %d", documentID),
			`SELECT `+versionColumns+` FROM document_versions
			  WHERE document_id = :document_id ORDER BY version DESC`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
			func(stmt *sqlite.Stmt) error {
				v, err := scanVersion(stmt)
				if err != nil {
					return err
				}
				out = append(out, v)
				return nil
			})
	})
	return out, err
}

// VersionByNumber reads one version of a document.
func (db *DB) VersionByNumber(ctx context.Context, documentID int64, number int) (domain.Version, error) {
	var v domain.Version
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return versionByNumber(conn, documentID, number, &v)
	})
	return v, err
}

func versionByNumber(conn *sqlite.Conn, documentID int64, number int, v *domain.Version) error {
	return one(conn, fmt.Sprintf("version %d of document %d", number, documentID),
		`SELECT `+versionColumns+` FROM document_versions
		  WHERE document_id = :document_id AND version = :version`,
		func(stmt *sqlite.Stmt) {
			stmt.SetInt64(":document_id", documentID)
			stmt.SetInt64(":version", int64(number))
		},
		func(stmt *sqlite.Stmt) error {
			var err error
			*v, err = scanVersion(stmt)
			return err
		})
}

// DraftForDocument returns the open working draft, or domain.ErrNotFound when
// every version is checked in. A partial unique index makes "the" draft a
// truth of the schema rather than of this query (0004_documents.sql).
func (db *DB) DraftForDocument(ctx context.Context, documentID int64) (domain.Version, error) {
	var v domain.Version
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return draftForDocument(conn, documentID, &v)
	})
	return v, err
}

func draftForDocument(conn *sqlite.Conn, documentID int64, v *domain.Version) error {
	return one(conn, fmt.Sprintf("the working draft of document %d", documentID),
		`SELECT `+versionColumns+` FROM document_versions
		  WHERE document_id = :document_id AND checked_in_at IS NULL`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*v, err = scanVersion(stmt)
			return err
		})
}

// latestVersionNumber returns the highest version number a document has, or 0
// when it has none.
func latestVersionNumber(conn *sqlite.Conn, documentID int64) (int, error) {
	var n int
	err := one(conn, fmt.Sprintf("the latest version of document %d", documentID),
		`SELECT COALESCE(MAX(version), 0) AS n FROM document_versions WHERE document_id = :document_id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
		func(stmt *sqlite.Stmt) error {
			n = int(stmt.GetInt64("n"))
			return nil
		})
	return n, err
}

// latestCheckedIn returns the newest checked-in version, or domain.ErrNotFound
// when the document has never been checked in.
func latestCheckedIn(conn *sqlite.Conn, documentID int64, v *domain.Version) error {
	return one(conn, fmt.Sprintf("the latest checked-in version of document %d", documentID),
		`SELECT `+versionColumns+` FROM document_versions
		  WHERE document_id = :document_id AND checked_in_at IS NOT NULL
		  ORDER BY version DESC LIMIT 1`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*v, err = scanVersion(stmt)
			return err
		})
}

func scanVersion(stmt *sqlite.Stmt) (domain.Version, error) {
	id := stmt.GetInt64("id")
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Version{}, fmt.Errorf("version %d: created_at: %w", id, err)
	}
	var checkedIn time.Time
	if s := nullText(stmt, "checked_in_at"); s != nil {
		if checkedIn, err = parseTime(*s); err != nil {
			return domain.Version{}, fmt.Errorf("version %d: checked_in_at: %w", id, err)
		}
	}
	v := domain.Version{
		ID:          id,
		DocumentID:  stmt.GetInt64("document_id"),
		Number:      int(stmt.GetInt64("version")),
		Title:       stmt.GetText("title"),
		Slug:        stmt.GetText("slug"),
		Content:     stmt.GetText("content"),
		CreatedBy:   stmt.GetInt64("created_by"),
		CreatedAt:   created,
		CheckedInAt: checkedIn,
	}
	if s := nullText(stmt, "cover_date"); s != nil {
		v.CoverDate = *s
	}
	if s := nullText(stmt, "note"); s != nil {
		v.Note = *s
	}
	return v, nil
}

// The state changes. Each of them is one transaction containing the check, the
// write, and the event (invariant 7), and each expresses its check as a
// condition on the UPDATE rather than as a read followed by a write.
//
// That is what makes PLAN.md M3 acceptance 3 true: two concurrent checkouts
// are two UPDATE statements against one row, and SQLite decides which of them
// finds the row unlocked. A read-then-write would let both reads see an
// unlocked document and both writes succeed, with the second silently taking
// the lock from the first.

// Checkout takes the edit lease and makes sure there is a working draft.
//
// A live lock held by anybody, including the caller, refuses: checking out
// something you already have is a mistake worth being told about, and the
// answer is a 409 rather than a silently extended lease. An expired lock does
// not refuse, because an expired lock is not a lock (acceptance 4).
//
// When every version is checked in, the next version number is opened as a
// draft copying the latest checked-in one. When a draft is already open --
// the document was created and never checked in, or a previous checkout was
// cancelled -- that draft is the one the caller gets back.
func (db *DB) Checkout(ctx context.Context, documentID, userID int64, now, expires time.Time, event domain.Event) (domain.Document, domain.Version, error) {
	var (
		doc domain.Document
		ver domain.Version
	)
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("checking out document %d", documentID), `
			UPDATE documents
			   SET locked_by = :user, lock_expires_at = :expires, updated_at = :now
			 WHERE id = :id
			   AND (locked_by IS NULL OR lock_expires_at IS NULL OR lock_expires_at <= :now)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":user", userID)
				stmt.SetText(":expires", formatTime(expires))
				stmt.SetText(":now", formatTime(now))
				stmt.SetInt64(":id", documentID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return lockedElsewhere(conn, documentID)
		}

		switch err := draftForDocument(conn, documentID, &ver); {
		case err == nil:
			// A draft was already open: the document was created and never
			// checked in, or somebody cancelled a checkout without reverting.
		case isNotFound(err):
			var latest domain.Version
			if err := latestCheckedIn(conn, documentID, &latest); err != nil {
				return err
			}
			n, err := latestVersionNumber(conn, documentID)
			if err != nil {
				return err
			}
			latest.ID = 0
			latest.Number = n + 1
			latest.Note = ""
			latest.CreatedBy = userID
			latest.CreatedAt = now
			latest.CheckedInAt = time.Time{}
			if ver, err = insertVersion(conn, latest); err != nil {
				return err
			}
			if err := setCurrentVersion(conn, documentID, ver.ID, now); err != nil {
				return err
			}
		default:
			return err
		}

		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = documentID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return documentByID(conn, documentID, &doc)
	})
	return doc, ver, err
}

// UpdateDraft writes the open working draft and extends the lease.
//
// The lease is extended by activity (DESIGN.md 5.1), and it is extended by the
// same statement that checks it, so an edit arriving one second before the
// lease runs out either takes the lock forward or is refused -- never both.
func (db *DB) UpdateDraft(ctx context.Context, documentID, userID int64, now, expires time.Time, updated domain.Version, event domain.Event) (domain.Document, domain.Version, error) {
	var (
		doc domain.Document
		ver domain.Version
	)
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		if err := holdLease(conn, documentID, userID, now, expires); err != nil {
			return err
		}

		err := run(conn, fmt.Sprintf("updating the draft of document %d", documentID), `
			UPDATE document_versions
			   SET title = :title, slug = :slug, cover_date = :cover_date, content = :content
			 WHERE document_id = :document_id AND checked_in_at IS NULL`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":title", updated.Title)
				stmt.SetText(":slug", updated.Slug)
				bindNullTime(stmt, ":cover_date", updated.CoverDate)
				stmt.SetText(":content", domain.NormalizeContent(updated.Content))
				stmt.SetInt64(":document_id", documentID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return fmt.Errorf("document %d has no open working draft: %w", documentID, domain.ErrConflict)
		}
		if err := draftForDocument(conn, documentID, &ver); err != nil {
			return err
		}

		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = documentID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return documentByID(conn, documentID, &doc)
	})
	return doc, ver, err
}

// Checkin closes the working draft and releases the lease.
//
// After this the version row is immutable: the trigger refuses every UPDATE
// touching a row whose checked_in_at is set, so a scheduled publish that pins
// this version id can trust it (invariant 8, acceptance 2).
func (db *DB) Checkin(ctx context.Context, documentID, userID int64, now time.Time, note string, event domain.Event) (domain.Document, domain.Version, error) {
	var (
		doc domain.Document
		ver domain.Version
	)
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		if err := releaseLease(conn, documentID, userID, now); err != nil {
			return err
		}

		err := run(conn, fmt.Sprintf("checking in document %d", documentID), `
			UPDATE document_versions
			   SET checked_in_at = :now, note = :note
			 WHERE document_id = :document_id AND checked_in_at IS NULL`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":now", formatTime(now))
				bindNullTime(stmt, ":note", note)
				stmt.SetInt64(":document_id", documentID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return fmt.Errorf("document %d has no open working draft: %w", documentID, domain.ErrConflict)
		}
		if err := latestCheckedIn(conn, documentID, &ver); err != nil {
			return err
		}
		if err := setCurrentVersion(conn, documentID, ver.ID, now); err != nil {
			return err
		}

		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = documentID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return documentByID(conn, documentID, &doc)
	})
	return doc, ver, err
}

// CancelCheckout releases the lease and leaves the draft alone.
//
// Cancelling a checkout is not discarding work. The draft survives with every
// edit in it, and the next checkout picks it up where it was left; throwing
// the changes away is Revert, which says so in its name.
func (db *DB) CancelCheckout(ctx context.Context, documentID, userID int64, now time.Time, event domain.Event) (domain.Document, error) {
	var doc domain.Document
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		if err := releaseLease(conn, documentID, userID, now); err != nil {
			return err
		}
		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = documentID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return documentByID(conn, documentID, &doc)
	})
	return doc, err
}

// RevertResult says what reverting did. The document is gone when its draft
// was its only version, which is acceptance 5 and the reason there is no
// version 0: nothing needs to know that reverting a never-saved document means
// deleting it, because the absence of a checked-in row says so.
type RevertResult struct {
	// Deleted is true when the whole document was removed.
	Deleted bool

	// Document is the document as it stands afterwards, zero when Deleted.
	Document domain.Document

	// Version is the checked-in version left as current, zero when Deleted.
	Version domain.Version
}

// Revert discards the open working draft.
//
// The event is written before the document is deleted, and it is written
// against the document's id, so a deleted document still has a history: the
// row is gone and the account of what happened to it is not. That is the whole
// point of an audit spine that is a table rather than a log.
func (db *DB) Revert(ctx context.Context, documentID, userID int64, now time.Time, event domain.Event) (RevertResult, error) {
	var out RevertResult
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		// Reverting is refused while somebody else is actively editing, and
		// permitted when the caller holds the lease or when nobody does. An
		// expired lease is nobody's.
		if err := revertable(conn, documentID, userID, now); err != nil {
			return err
		}

		var draft domain.Version
		if err := draftForDocument(conn, documentID, &draft); err != nil {
			if isNotFound(err) {
				return fmt.Errorf("document %d has no open working draft to revert: %w", documentID, domain.ErrConflict)
			}
			return err
		}

		// The pointer is cleared before the row it points at is deleted;
		// current_version_id is a real foreign key.
		if err := setCurrentVersion(conn, documentID, 0, now); err != nil {
			return err
		}
		if err := run(conn, fmt.Sprintf("discarding the draft of document %d", documentID),
			`DELETE FROM document_versions WHERE id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", draft.ID) }, nil); err != nil {
			return err
		}

		var latest domain.Version
		switch err := latestCheckedIn(conn, documentID, &latest); {
		case err == nil:
			if err := setCurrentVersion(conn, documentID, latest.ID, now); err != nil {
				return err
			}
			if err := releaseLease(conn, documentID, userID, now); err != nil && !isConflict(err) {
				// The lease may already be nobody's, which is not an error
				// here: reverting an unlocked document is permitted.
				return err
			}
			out.Version = latest
		case isNotFound(err):
			out.Deleted = true
		default:
			return err
		}

		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = documentID
		if event.Payload == nil {
			event.Payload = map[string]any{}
		}
		event.Payload["deleted"] = out.Deleted
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}

		if out.Deleted {
			return run(conn, fmt.Sprintf("deleting document %d", documentID),
				`DELETE FROM documents WHERE id = :id`,
				func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", documentID) }, nil)
		}
		return documentByID(conn, documentID, &out.Document)
	})
	return out, err
}

// holdLease requires that userID holds a live lease and extends it. The check
// and the extension are one statement, so a lease cannot expire between them.
func holdLease(conn *sqlite.Conn, documentID, userID int64, now, expires time.Time) error {
	err := run(conn, fmt.Sprintf("extending the lease on document %d", documentID), `
		UPDATE documents
		   SET lock_expires_at = :expires, updated_at = :now
		 WHERE id = :id AND locked_by = :user AND lock_expires_at > :now`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":expires", formatTime(expires))
			stmt.SetText(":now", formatTime(now))
			stmt.SetInt64(":id", documentID)
			stmt.SetInt64(":user", userID)
		}, nil)
	if err != nil {
		return err
	}
	if conn.Changes() == 0 {
		return lockedElsewhere(conn, documentID)
	}
	return nil
}

// releaseLease requires that userID holds a live lease and clears it.
func releaseLease(conn *sqlite.Conn, documentID, userID int64, now time.Time) error {
	err := run(conn, fmt.Sprintf("releasing the lease on document %d", documentID), `
		UPDATE documents
		   SET locked_by = NULL, lock_expires_at = NULL, updated_at = :now
		 WHERE id = :id AND locked_by = :user AND lock_expires_at > :now`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":now", formatTime(now))
			stmt.SetInt64(":id", documentID)
			stmt.SetInt64(":user", userID)
		}, nil)
	if err != nil {
		return err
	}
	if conn.Changes() == 0 {
		return lockedElsewhere(conn, documentID)
	}
	return nil
}

// revertable requires that nobody but userID holds a live lease.
func revertable(conn *sqlite.Conn, documentID, userID int64, now time.Time) error {
	err := run(conn, fmt.Sprintf("claiming document %d for a revert", documentID), `
		UPDATE documents
		   SET updated_at = :now
		 WHERE id = :id
		   AND (locked_by IS NULL OR locked_by = :user OR lock_expires_at IS NULL OR lock_expires_at <= :now)`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":now", formatTime(now))
			stmt.SetInt64(":id", documentID)
			stmt.SetInt64(":user", userID)
		}, nil)
	if err != nil {
		return err
	}
	if conn.Changes() == 0 {
		return lockedElsewhere(conn, documentID)
	}
	return nil
}

// lockedElsewhere turns "the conditional UPDATE matched nothing" into the
// error that says why.
//
// Zero rows changed means one of two things: the document is not there, or
// somebody else has it. Distinguishing them costs two reads and is worth it,
// because "not found" and "checked out by Alice until 16:20" send a person to
// two different places.
//
// The holder is named, never numbered: this message reaches a client in a
// problem document, and an internal primary key never appears in one
// (invariant 10).
func lockedElsewhere(conn *sqlite.Conn, documentID int64) error {
	var d domain.Document
	if err := documentByID(conn, documentID, &d); err != nil {
		return err
	}
	if d.Lock.UserID == 0 {
		return fmt.Errorf("document %s is not checked out: %w", d.UID, domain.ErrConflict)
	}

	holder := "somebody else"
	var u domain.User
	if err := userByID(conn, d.Lock.UserID, &u); err == nil {
		holder = u.Name
	}
	return fmt.Errorf("document %s is checked out by %s until %s: %w",
		d.UID, holder, d.Lock.ExpiresAt.Format(time.RFC3339), domain.ErrConflict)
}
