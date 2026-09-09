-- Copyright (c) 2026 Michael D Henderson.
--
-- 0011: comments and approvals get the API in front of them (DESIGN.md 5.5,
-- PLAN.md M11).
--
-- No tables. Both landed in 0005, one milestone before their routes, because
-- approvals_met and comments_resolved are two of the eight guards and a guard
-- whose data nothing can produce is a guard nobody has tested (invariant 6).
-- What arrives with M11 is the API, and the two statements below are what the
-- API makes true.

-- The default workflow's "review" state now asks for an approval.
--
-- 0005 set required_approvals to 0 on every state and said why: the guard was
-- enforced, but nothing could record an approval, and a default process no
-- editor can move a document through is not one to ship. M11 is the milestone
-- that satisfies it, so this is where the column is raised.
--
-- One and not two. "Two distinct people must approve" is what the UNIQUE
-- constraint on (version_id, state, user_id) makes cheap, and it is what a
-- large desk will configure -- but it is a value, not a default: a newsroom
-- with one editor on a Sunday would find the default process stuck for exactly
-- the reason 0005 refused to ship a stuck one. Raising it further is one
-- UPDATE against this column, which is the whole point of it being a column.
--
-- Matched by the states it joins rather than by id, for the reason 0010 gives:
-- an installation that has edited its copy of the default workflow keeps its
-- edits and still gets the process the state machine already implied.
UPDATE workflow_states
   SET required_approvals = 1
 WHERE workflow_id = (SELECT id FROM workflows WHERE uid = '00000000000000000000000001')
   AND slug = 'review'
   AND required_approvals = 0;

-- "Every comment on this document, oldest first", which is what the thread
-- listing M11 adds asks on every document view.
--
-- comments_open, from 0005, answers the guard's question and only the guard's:
-- it is partial on "resolved_at IS NULL", so a listing that wants the resolved
-- ones too cannot use it and would scan every comment in the system. This is
-- 0007's reasoning again, one table over -- an index arrives with the query
-- that seeks on it, not with the table.
CREATE INDEX comments_document ON comments(document_id, id);
