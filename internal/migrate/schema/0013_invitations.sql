-- Copyright (c) 2026 Michael D Henderson.
--
-- 0013: invitations (issue #6).
--
-- Registration is invite-only and there is no self-service account creation.
-- An administrator creates a row here; the system hands back a link once; the
-- person at the other end redeems it, which creates the user and nothing else
-- -- no session (issue #6, "Redemption creates the account and redirects to
-- /login").
--
-- Three properties of this table carry the whole design and each of them is a
-- decision rather than a default.
--
--   * The token is stored as a SHA-256 and never in the clear, which is what
--     sessions.token_sha256 already does (DESIGN.md 5.5): read access to this
--     file cannot mint a working link.
--
--   * Expiry is derived, never stored as a status. "expires_at < now" is
--     computed by the listing query and by the redemption check, so nothing
--     runs at the 48-hour mark, there is no sweep job, and there is no second
--     source of truth for a fact the timestamp already carries.
--
--   * No row is ever deleted. An invitation is an event subject -- "who
--     invited this person, when, and did they ever accept" has to stay
--     answerable -- and a row that vanishes cannot anchor an audit trail, for
--     the reason internal/domain/publish.go:44 gives for having no
--     SubjectResource.
CREATE TABLE invitations (
    id    INTEGER PRIMARY KEY,
    uid   TEXT    NOT NULL UNIQUE,          -- external identifier (invariant 10)

    -- The address the invitation is for, folded by domain.NormalizeEmail
    -- before it arrives. It is not a foreign key to users: the point of an
    -- invitation is that the account does not exist yet.
    email TEXT    NOT NULL,

    -- The SHA-256 of the token in the link, NULL once the row is terminal.
    --
    -- Nullable because clearing it is the last act of every path that settles
    -- a row, so that historical rows carry no sensitive column at all. An
    -- invitation that expires and is never visited again keeps its hash until
    -- something touches it, which is safe and is worth saying why: the derived
    -- expiry check runs before the hash is ever compared, so the hash on an
    -- expired row is already inert, and the value is a SHA-256 of a
    -- high-entropy token either way. Clearing it is defence in depth and not
    -- the thing standing between an attacker and an account.
    token_sha256 TEXT,

    -- pending is the only non-terminal status. There is deliberately no
    -- 'expired': expiry is derived from expires_at, and a stored copy would
    -- need something to run at the 48-hour mark to write it.
    --
    -- 'superseded' is what a re-invitation does to the row it replaces. It
    -- says something true that 'revoked' would not -- nobody decided against
    -- this person -- and it is what keeps the partial index below from
    -- refusing a second invitation to an address whose first one lapsed.
    status TEXT NOT NULL DEFAULT 'pending'
           CHECK (status IN ('pending', 'redeemed', 'revoked', 'superseded')),

    expires_at TEXT NOT NULL,

    -- Why it was revoked, when whoever revoked it said. It is a column rather
    -- than a second verb: "force this to expire" and "revoke this" have the
    -- same observable effect, and the only thing that distinguishes them is
    -- the sentence in the audit trail, which belongs here.
    reason TEXT,

    invited_by INTEGER REFERENCES users(id),  -- who sent it
    user_id    INTEGER REFERENCES users(id),  -- the account redemption created

    created_at TEXT NOT NULL,
    settled_at TEXT                           -- when it stopped being pending
) STRICT;

-- One live invitation per address.
--
-- This is the one-open-draft index (document_versions_one_draft) and 0006's
-- two workflow indexes again, and it has an interaction with derived expiry
-- that is worth stating rather than discovering: a lapsed invitation still has
-- status 'pending', so this index reads a dead row as a live one. SQLite
-- requires a partial index's WHERE clause to be deterministic, so narrowing it
-- with "AND expires_at > now" is not available.
--
-- What resolves it is that creating an invitation supersedes any row this
-- index would collide with, in the same transaction (store.CreateInvitation).
-- The index is then a backstop under a race rather than something an
-- administrator ever meets, detected by result code and never by message text
-- (invariant 11).
CREATE UNIQUE INDEX invitations_one_pending ON invitations (email) WHERE status = 'pending';

-- "Which invitation is this link for", which is the only question redemption
-- asks. Partial on the rows that still have a hash, because a settled row is
-- never an answer to it.
CREATE INDEX invitations_token ON invitations (token_sha256) WHERE token_sha256 IS NOT NULL;

-- The administrator's list: pending by default, newest first, with the rest
-- available on request. An index arrives with the query that seeks on it,
-- which is 0007's rule.
CREATE INDEX invitations_listing ON invitations (status, id DESC);
