# Walkthrough

A narrative orientation to the running system, for a reader who has not used it.
`docs/DESIGN.md` says what is being built and why; this says what a Thursday
looks like once it is built.

## Six Desks, One Story

<https://claude.ai/code/artifact/6bd3fc5d-612e-4487-8cea-c8c0a1b4869e>

One story moving from a blank page to a live URL over a single day, followed
through six people who hold five different privileges: Betty (`viewer`, read),
Jan, Murray and Mary (`writer`, edit), Lou (`editor`, create), and Clark
(`admin`, publish).

It covers the ground the reference documents state as rules, in the order
somebody actually meets them: the greyed action bar and why a refused move is
drawn rather than omitted (invariant 5), the edit lease and why a held lock is a
409 and not a 403, check-in as the only moment content is validated, the three
subresources that deliberately need no lease, `Submit`'s three guards and two
effects, approval bound to a version, the related-asset cascade gathered at
schedule time (invariant 8), and `Revise` assigning to whoever pulls it back.

Every guard, effect, privilege and status code in it is drawn from the code and
from the workflow seeded by `internal/migrate/schema/0005_workflow.sql`. The
times, the names and the story are invented.

The page is public: the link above resolves for anyone who has it, with no
Claude account needed. Treat it as published — it is a reasonable thing to
hand to somebody evaluating the system, and a poor place to put anything this
repository would not say in the open.
