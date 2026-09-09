// Copyright (c) 2026 Michael D Henderson.

// Package jobs is the background queue: leases, the worker loop, and the job
// kinds (DESIGN.md 9, PLAN.md M6).
//
// Three ideas carry the design, and all three are corrections of the system we
// learned from.
//
// The lease expires on its own. That system marked a row "executing" with no
// lease and no heartbeat, so a worker that died mid-job left the row marked
// executing forever and recovery was a manual UPDATE by whoever noticed. Here
// nobody has to notice: a claim writes a deadline, every question about the
// lease is asked against an instant, and a worker that never comes back costs
// one lease duration.
//
// The claim is one statement. Choosing a job and marking it taken happen in
// the same UPDATE ... RETURNING, so twenty workers racing for one ready job
// produce one winner and nineteen empty results -- a property of the statement
// rather than of any lock this process happens to hold.
//
// Priority is honoured before schedule. It is what lets a fifty-thousand
// document republish run at priority 5 without blocking an editor pressing
// Publish, and it is only true if the index agrees with the ORDER BY, which is
// what 0008 is about.
//
// Permitted imports: internal/domain, internal/store, internal/clock,
// internal/ids, internal/events.
package jobs
