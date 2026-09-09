// Copyright (c) 2026 Michael D Henderson.

package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
)

// The alert dispatcher (DESIGN.md 6.4 and 10, PLAN.md M12).
//
// Alert evaluation happens after commit, driven off the event row, so that a
// failing notification cannot roll back an editorial action. That sentence
// decides the shape of everything below.
//
// "After commit" could have been a call at the end of each service method.
// There are thirty-odd places in this system that write an event, and thirty
// places to remember something is thirty places to forget it -- which is the
// failure mode invariant 7 exists to prevent, reintroduced one layer up. So
// the dispatcher reads the events table instead: alert_cursor says how far it
// has got, a batch is evaluated and recorded in one transaction, and an event
// written by cmsdb, by a job worker, or by a route nobody has written yet is
// picked up without anybody having wired it in.
//
// The cost is latency -- an alert arrives within a poll rather than within a
// request -- and that is the right trade for a notification. The benefit is
// that "every state change is alertable" is a property of the shape rather
// than a checklist.

// The dispatcher's defaults.
const (
	// DefaultBatch is how many events one pass evaluates. It bounds the
	// transaction rather than the work: a dispatcher that has fallen behind
	// catches up one batch per pass, and a pass that found a full batch does
	// not wait before the next one.
	DefaultBatch = 100

	// DefaultPoll is how long an idle dispatcher waits before looking again.
	// It is longer than the job pool's idle delay because nobody is watching
	// a notification arrive the way they watch a publish run.
	DefaultPoll = 2 * time.Second
)

// Delivery is one notification on its way out: which rule fired, about what,
// and to whom.
type Delivery struct {
	Rule  domain.AlertRule
	Event domain.Event

	// Recipient is the person being told, or the zero User when the rule's
	// target is a bare address that belongs to nobody with an account.
	Recipient domain.User

	// Address is where an e-mail goes. It is empty on the in-app channel,
	// which delivers to a row rather than to an address.
	Address string
}

// Store is what the dispatcher needs from the database.
//
// It is an interface rather than *store.DB, and the reason is a dependency
// that would otherwise point both ways: internal/store's own tests use this
// package's event-type constants, which they should -- a fixture that spelled
// "document.approved" as a string literal is a fixture one typo away from
// asserting nothing -- and a package that imports the package whose tests
// import it cannot be built. Naming the seven methods here inverts it, and
// *store.DB satisfies it without being told.
//
// It is also what lets a test drive the dispatcher against a store that fails
// on demand, which is how "a failing channel does not roll back the transition
// that caused it" is asserted from both sides (PLAN.md M12 acceptance 4).
type Store interface {
	// PendingAlertEvents reads the events not yet evaluated, oldest first,
	// with the cursor they were read at.
	PendingAlertEvents(ctx context.Context, limit int) (domain.AlertBatch, error)

	// ActiveAlertRules is every rule that is switched on.
	ActiveAlertRules(ctx context.Context) ([]domain.AlertRule, error)

	// Deliver records a batch's notifications and advances the cursor past
	// it, in one transaction. It reports how many rows it wrote and whether
	// the cursor moved; a false means another dispatcher took the batch.
	Deliver(ctx context.Context, cursor, next int64, notes []domain.NewNotification, now time.Time) (int, bool, error)

	// The four reads that turn an event row into facts and a target into
	// people.
	UserByID(ctx context.Context, id int64) (domain.User, error)
	UserByUID(ctx context.Context, uid string) (domain.User, error)
	UsersWithRole(ctx context.Context, slug string) ([]domain.User, error)
	DocumentByID(ctx context.Context, id int64) (domain.Document, error)
	VersionByID(ctx context.Context, id int64) (domain.Version, error)
}

// Deliverer carries a notification out of this process (PLAN.md M12).
//
// The in-app channel deliberately does not go through it: an in-app
// notification is a row in this database, written inside the transaction that
// advances the cursor, and a Deliverer that could fail while holding that
// transaction open is exactly the thing DESIGN.md 6.4 forbids. What this
// interface is for is everything that leaves -- e-mail today, whatever an
// installation adds later -- and its whole contract is that it runs after the
// commit and that its failure is logged rather than propagated.
type Deliverer interface {
	Deliver(ctx context.Context, d Delivery) error
}

// LogDeliverer is the default e-mail channel: it writes a line and returns.
//
// A CMS that sent mail from the box it was installed on would need a relay, a
// bounce policy, and a queue of its own, and none of that is what M12 is
// about. What the milestone needs is that the channel is behind an interface
// so an installation can supply the real one, and that the default says out
// loud what it would have sent (PLAN.md M12).
type LogDeliverer struct {
	Log *slog.Logger
}

// Deliver writes one line per notification. It never fails, which is the
// honest thing for a channel that never sends: reporting an error would make
// every default installation log a failure for working correctly.
func (d LogDeliverer) Deliver(_ context.Context, n Delivery) error {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log.Info("notification (logging channel; nothing was sent)",
		"rule", n.Rule.Name,
		"channel", string(n.Rule.Channel),
		"to", n.Address,
		"event", n.Event.Type,
		"subject_kind", n.Event.SubjectKind,
		"subject_id", n.Event.SubjectID,
	)
	return nil
}

// DispatcherOptions configure a Dispatcher. Everything is resolved before
// NewDispatcher is called; this package reads no flags and no environment.
type DispatcherOptions struct {
	// DB and Clock are required.
	DB    Store
	Clock clock.Clock

	// Logger receives the structured log. A nil Logger discards.
	Logger *slog.Logger

	// Batch is how many events one pass evaluates; zero means DefaultBatch.
	Batch int

	// Poll is how long an idle pass waits; zero means DefaultPoll.
	Poll time.Duration

	// Deliverers are the channels that leave this process, by name. A channel
	// with no deliverer is logged once per delivery rather than silently
	// dropped: a rule that names a channel this binary cannot deliver is a
	// rule somebody believes is working.
	//
	// A nil map means the default: LogDeliverer on the e-mail channel.
	Deliverers map[domain.AlertChannel]Deliverer

	// Wait is the idle delay, a seam for tests. It returns false when ctx
	// ended before the delay elapsed. A nil Wait uses a timer.
	Wait func(ctx context.Context, d time.Duration) bool
}

// Dispatcher evaluates alert rules over the event stream.
type Dispatcher struct {
	db         Store
	clock      clock.Clock
	log        *slog.Logger
	batch      int
	poll       time.Duration
	deliverers map[domain.AlertChannel]Deliverer
	wait       func(ctx context.Context, d time.Duration) bool
}

// NewDispatcher builds a dispatcher over an open database.
func NewDispatcher(opts DispatcherOptions) (*Dispatcher, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("events: no database")
	}
	if opts.Clock == nil {
		return nil, fmt.Errorf("events: no clock; every component that needs the time is given one (invariant 3)")
	}
	d := &Dispatcher{
		db:         opts.DB,
		clock:      opts.Clock,
		log:        opts.Logger,
		batch:      opts.Batch,
		poll:       opts.Poll,
		deliverers: opts.Deliverers,
		wait:       opts.Wait,
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if d.batch <= 0 {
		d.batch = DefaultBatch
	}
	if d.poll <= 0 {
		d.poll = DefaultPoll
	}
	if d.deliverers == nil {
		d.deliverers = map[domain.AlertChannel]Deliverer{
			domain.ChannelEmail: LogDeliverer{Log: d.log},
		}
	}
	if d.wait == nil {
		d.wait = sleep
	}
	return d, nil
}

// Now reads the injected clock. Nothing in this package calls time.Now
// (invariant 3).
func (d *Dispatcher) Now() time.Time { return d.clock.Now().UTC() }

// Result is what one pass did.
type Result struct {
	// Events is how many event rows were evaluated, Notified how many
	// notification rows were written, and Delivered how many external
	// deliveries were attempted. Failed counts the ones that returned an
	// error, which is a number a pass reports rather than fails on.
	Events    int
	Notified  int
	Delivered int
	Failed    int
}

// Run evaluates until ctx ends. It satisfies the server's Background, so the
// dispatcher stops on the one shutdown path with the job workers
// (invariant 17).
//
// It returns nil on an ordinary shutdown. A pass that fails is logged and
// retried, for the reason a worker's failed claim is: one unreadable row is
// not a reason to stop serving HTTP.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.log.Info("alert dispatcher starting", "batch", d.batch, "poll", d.poll.String())
	for {
		if ctx.Err() != nil {
			d.log.Info("alert dispatcher stopped")
			return nil
		}
		res, err := d.Once(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			d.log.Error("evaluating alert rules failed", "error", err)
		case res.Events == d.batch:
			// A full batch means there is more waiting. Go straight back
			// round rather than sleeping through a backlog.
			continue
		}
		if !d.wait(ctx, d.poll) {
			d.log.Info("alert dispatcher stopped")
			return nil
		}
	}
}

// Once evaluates one batch of events and returns what it did.
//
// It is exported because it is what a test drives and what makes the
// dispatcher's behaviour assertable without a running server: a test writes an
// event through the ordinary path, calls this, and reads the inbox.
func (d *Dispatcher) Once(ctx context.Context) (Result, error) {
	batch, err := d.db.PendingAlertEvents(ctx, d.batch)
	if err != nil {
		return Result{}, err
	}
	if len(batch.Events) == 0 {
		return Result{}, nil
	}
	res := Result{Events: len(batch.Events)}

	rules, err := d.db.ActiveAlertRules(ctx)
	if err != nil {
		return res, err
	}

	// Nothing to evaluate is still a batch to acknowledge. An installation
	// with no rules must not accumulate a million-row backlog against the day
	// somebody writes one.
	var (
		notes      []domain.NewNotification
		deliveries []Delivery
	)
	if len(rules) > 0 {
		notes, deliveries, err = d.evaluate(ctx, batch.Events, rules)
		if err != nil {
			return res, err
		}
	}

	now := d.Now()
	written, moved, err := d.db.Deliver(ctx, batch.Cursor, batch.Next(), notes, now)
	if err != nil {
		return res, err
	}
	if !moved {
		// Another dispatcher delivered this batch. Nothing was written here,
		// and nothing leaves this process either: whoever won the swap is
		// doing that.
		d.log.Debug("another dispatcher took this batch",
			"cursor", batch.Cursor, "events", len(batch.Events))
		return Result{}, nil
	}
	res.Notified = written

	// Everything that leaves the process happens here, after the commit, and
	// a failure is counted and logged rather than returned (PLAN.md M12
	// acceptance 4). The transition that caused the notification committed a
	// long time ago; there is nothing left for a channel to roll back.
	for _, n := range deliveries {
		res.Delivered++
		deliverer, ok := d.deliverers[n.Rule.Channel]
		if !ok {
			res.Failed++
			d.log.Error("no deliverer for an alert channel",
				"rule", n.Rule.Name, "channel", string(n.Rule.Channel))
			continue
		}
		if err := deliverer.Deliver(ctx, n); err != nil {
			res.Failed++
			d.log.Error("delivering a notification failed",
				"rule", n.Rule.Name, "channel", string(n.Rule.Channel),
				"to", n.Address, "event", n.Event.Type, "error", err)
		}
	}
	return res, nil
}

// evaluate matches every event against every rule of its type and resolves the
// recipients of the ones that matched.
func (d *Dispatcher) evaluate(ctx context.Context, batch []domain.Event, rules []domain.AlertRule) ([]domain.NewNotification, []Delivery, error) {
	byType := map[string][]domain.AlertRule{}
	for _, r := range rules {
		byType[r.EventType] = append(byType[r.EventType], r)
	}

	cache := newResolver(d.db)
	var (
		notes      []domain.NewNotification
		deliveries []Delivery
	)
	for _, e := range batch {
		matching := byType[e.Type]
		if len(matching) == 0 {
			continue
		}
		// The facts are built once per event and not once per rule: five
		// rules about document.transitioned ask the same three questions of
		// the same document.
		facts, err := d.facts(ctx, e, cache)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range matching {
			if !r.Matches(facts) {
				continue
			}
			recipients, err := d.recipients(ctx, r, cache)
			if err != nil {
				return nil, nil, err
			}
			for _, who := range recipients {
				if r.Channel == domain.ChannelInApp {
					uid, err := ids.New(d.Now())
					if err != nil {
						return nil, nil, err
					}
					notes = append(notes, domain.NewNotification{
						UID: uid, UserID: who.Recipient.ID, EventID: e.ID,
						RuleID: r.ID, CreatedAt: d.Now(),
					})
					continue
				}
				who.Rule, who.Event = r, e
				deliveries = append(deliveries, who)
			}
		}
	}
	return notes, deliveries, nil
}

// recipients resolves a rule's target into the people it names.
//
// A target that names nobody -- a deleted user, an empty role -- notifies
// nobody and is logged rather than failing the batch. A rule pointing at
// somebody who has left is a configuration problem, and taking the whole
// dispatcher down over it would mean one stale rule stops every alert in the
// system.
func (d *Dispatcher) recipients(ctx context.Context, r domain.AlertRule, cache *resolver) ([]Delivery, error) {
	target, err := r.ParsedTarget()
	if err != nil {
		d.log.Error("an alert rule has a target this binary cannot resolve",
			"rule", r.Name, "target", r.Target, "error", err)
		return nil, nil
	}

	switch target.Kind {
	case domain.TargetEmail:
		return []Delivery{{Address: target.Name}}, nil

	case domain.TargetUser:
		u, err := cache.userByUID(ctx, target.Name)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				d.log.Warn("an alert rule names a user who no longer exists",
					"rule", r.Name, "target", r.Target)
				return nil, nil
			}
			return nil, err
		}
		return []Delivery{{Recipient: u, Address: u.Email}}, nil

	case domain.TargetRole:
		users, err := cache.usersWithRole(ctx, target.Name)
		if err != nil {
			return nil, err
		}
		out := make([]Delivery, 0, len(users))
		for _, u := range users {
			out = append(out, Delivery{Recipient: u, Address: u.Email})
		}
		return out, nil

	default:
		return nil, nil
	}
}

// facts assembles the three sources a condition resolves a field from
// (DESIGN.md 10): the acting user, the event payload, then the subject.
//
// The actor's keys are spelled "actor_uid", "actor_email", "actor_name" rather
// than "uid", "email", "name". The resolution order is what the design asks
// for and it is implemented in domain.Facts.Resolve, but a rule about "email"
// that silently meant the actor's rather than the subject's would be a rule
// that reads one way and behaves another. Ambient facts get names that say
// they are ambient; the overlap that matters -- and the one PLAN.md M12
// acceptance 5 is about -- is the payload's over the subject's, where the
// payload is what was true at the moment and the subject is what is true now.
func (d *Dispatcher) facts(ctx context.Context, e domain.Event, cache *resolver) (domain.Facts, error) {
	f := domain.Facts{Payload: e.Payload}

	if e.ActorID != 0 {
		u, err := cache.userByID(ctx, e.ActorID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			// The actor has been removed. The event survives them, and a rule
			// about the actor simply does not match: an unresolved field
			// fails every operator.
		case err != nil:
			return domain.Facts{}, err
		default:
			f.Actor = map[string]any{
				"actor_uid":   u.UID,
				"actor_email": u.Email,
				"actor_name":  u.Name,
			}
		}
	}

	subject, err := d.subject(ctx, e, cache)
	if err != nil {
		return domain.Facts{}, err
	}
	f.Subject = subject
	return f, nil
}

// subject reads the row an event is about and renders it as facts.
//
// Two kinds are spelled out and the rest carry only their kind. A document is
// the subject a rule is nearly always about, and a user is the other thing a
// person writes a rule against; a job, a category, or an output channel puts
// everything a rule would ask for into its payload already, which resolves
// first anyway.
func (d *Dispatcher) subject(ctx context.Context, e domain.Event, cache *resolver) (map[string]any, error) {
	out := map[string]any{"subject_kind": e.SubjectKind}

	switch e.SubjectKind {
	case domain.SubjectDocument:
		doc, err := cache.document(ctx, e.SubjectID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// A document reverted out of existence, or deleted. The event
				// is still worth alerting on and its payload still carries
				// what happened.
				return out, nil
			}
			return nil, err
		}
		out["uid"] = doc.UID
		out["kind"] = doc.Kind
		out["state"] = doc.State
		out["element_type"] = doc.ElementTypeKey
		out["site"] = doc.SiteID
		out["category"] = doc.CategoryPath
		if doc.AssignedTo != 0 {
			if u, err := cache.userByID(ctx, doc.AssignedTo); err == nil {
				out["assignee"] = u.UID
				out["assignee_email"] = u.Email
			}
		}
		if doc.CurrentVersionID != 0 {
			if v, err := cache.version(ctx, doc.CurrentVersionID); err == nil {
				out["title"] = v.Title
				out["slug"] = v.Slug
				out["version"] = v.Number
			}
		}

	case domain.SubjectUser:
		u, err := cache.userByID(ctx, e.SubjectID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return out, nil
			}
			return nil, err
		}
		out["uid"] = u.UID
		out["email"] = u.Email
		out["name"] = u.Name
	}
	return out, nil
}

// resolver caches the rows one pass reads more than once. A batch of fifty
// events is usually two or three people and a handful of documents.
type resolver struct {
	db Store

	usersByID  map[int64]domain.User
	usersByUID map[string]domain.User
	roles      map[string][]domain.User
	documents  map[int64]domain.Document
	versions   map[int64]domain.Version
}

func newResolver(db Store) *resolver {
	return &resolver{
		db:         db,
		usersByID:  map[int64]domain.User{},
		usersByUID: map[string]domain.User{},
		roles:      map[string][]domain.User{},
		documents:  map[int64]domain.Document{},
		versions:   map[int64]domain.Version{},
	}
}

func (c *resolver) userByID(ctx context.Context, id int64) (domain.User, error) {
	if hit, ok := c.usersByID[id]; ok {
		return hit, nil
	}
	u, err := c.db.UserByID(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	c.usersByID[id] = u
	c.usersByUID[u.UID] = u
	return u, nil
}

func (c *resolver) userByUID(ctx context.Context, uid string) (domain.User, error) {
	if hit, ok := c.usersByUID[uid]; ok {
		return hit, nil
	}
	u, err := c.db.UserByUID(ctx, uid)
	if err != nil {
		return domain.User{}, err
	}
	c.usersByUID[uid] = u
	c.usersByID[u.ID] = u
	return u, nil
}

func (c *resolver) usersWithRole(ctx context.Context, slug string) ([]domain.User, error) {
	if hit, ok := c.roles[slug]; ok {
		return hit, nil
	}
	users, err := c.db.UsersWithRole(ctx, slug)
	if err != nil {
		return nil, err
	}
	c.roles[slug] = users
	return users, nil
}

func (c *resolver) document(ctx context.Context, id int64) (domain.Document, error) {
	if hit, ok := c.documents[id]; ok {
		return hit, nil
	}
	doc, err := c.db.DocumentByID(ctx, id)
	if err != nil {
		return domain.Document{}, err
	}
	c.documents[id] = doc
	return doc, nil
}

func (c *resolver) version(ctx context.Context, id int64) (domain.Version, error) {
	if hit, ok := c.versions[id]; ok {
		return hit, nil
	}
	v, err := c.db.VersionByID(ctx, id)
	if err != nil {
		return domain.Version{}, err
	}
	c.versions[id] = v
	return v, nil
}

// sleep is the default idle wait. It reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
