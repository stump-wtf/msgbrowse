package store

import "context"

// schemaVersion is the current schema revision, recorded in SQLite's
// `user_version` pragma. On Open, the migrations runner brings any older
// database forward to this version. Bump it and append a migration whenever the
// schema changes.
const schemaVersion = 20

// SchemaVersion returns the schema revision this binary expects (and migrates a
// database forward to on Open). Read-only callers — notably `msgbrowse doctor` —
// compare it against a database's PRAGMA user_version to report drift.
func SchemaVersion() int { return schemaVersion }

// UserVersion returns the database's recorded schema version (PRAGMA
// user_version). After a successful Open this equals SchemaVersion(); doctor
// reads it directly so it can report the value without re-deriving it.
func (s *Store) UserVersion(ctx context.Context) (int, error) {
	return readUserVersion(ctx, s.db)
}

// migrations is the ordered list of per-version migrations applied on Open.
// Each entry's index is its version (1-based; index 0 is unused).
//
// Invariant: every migration MUST be idempotent within its version transition.
// The runner wraps each entry in a transaction and only sets `user_version`
// after the transaction commits.
//
// Design notes:
//   - v1 lays down the original Signal-only schema (conversations / messages /
//     attachments / links / snapshots / ingest_state / ingest_runs / FTS5
//     virtual table + triggers).
//   - v2 introduces the unified contacts model (`contacts` and
//     `contact_identifiers`) and adds a `source` column to conversations,
//     messages, and ingest_runs so the store can hold data from Signal AND
//     iMessage (and future sources) at once. Existing rows are stamped
//     source='signal' and each Signal conversation is bootstrapped with a
//     contact and identifier; see internal/source for the canonical names.
var migrations = []string{
	0:  "", // unused; versions are 1-based
	1:  schemaV1,
	2:  schemaV2,
	3:  schemaV3,
	4:  schemaV4,
	5:  schemaV5,
	6:  schemaV6,
	7:  schemaV7,
	8:  schemaV8,
	9:  schemaV9,
	10: schemaV10,
	11: schemaV11,
	12: schemaV12,
	13: schemaV13,
	14: schemaV14,
	15: schemaV15,
	16: schemaV16,
	17: schemaV17,
	18: schemaV18,
	19: schemaV19,
	20: schemaV20,
}

// schemaV1 is the initial Signal-only schema. It is preserved verbatim so a
// fresh database walks through the same sequence of changes a long-lived one
// did, which makes reasoning about either trivial.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS conversations (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS messages (
    id              INTEGER PRIMARY KEY,
    hash            TEXT    NOT NULL UNIQUE,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    ts              TEXT    NOT NULL,
    ts_unix         INTEGER NOT NULL,
    sender          TEXT    NOT NULL,
    body            TEXT    NOT NULL,
    is_system       INTEGER NOT NULL DEFAULT 0,
    seq             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_conv_ts ON messages(conversation_id, ts_unix);
CREATE INDEX IF NOT EXISTS idx_messages_sender  ON messages(sender);
CREATE INDEX IF NOT EXISTS idx_messages_ts_unix ON messages(ts_unix);

CREATE TABLE IF NOT EXISTS attachments (
    id            INTEGER PRIMARY KEY,
    message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,
    rel_path      TEXT    NOT NULL,
    original_name TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_kind    ON attachments(kind);

CREATE TABLE IF NOT EXISTS links (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    url        TEXT    NOT NULL,
    domain     TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_links_message ON links(message_id);
CREATE INDEX IF NOT EXISTS idx_links_domain  ON links(domain);

CREATE TABLE IF NOT EXISTS snapshots (
    id          INTEGER PRIMARY KEY,
    filename    TEXT    NOT NULL UNIQUE,
    taken_at    TEXT    NOT NULL,
    taken_unix  INTEGER NOT NULL,
    size_bytes  INTEGER NOT NULL,
    tier        TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS ingest_state (
    conversation_id  INTEGER PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    rel_path         TEXT    NOT NULL,
    mtime_unix       INTEGER NOT NULL,
    size_bytes       INTEGER NOT NULL,
    content_hash     TEXT    NOT NULL,
    message_count    INTEGER NOT NULL,
    last_ingested_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS ingest_runs (
    id                     INTEGER PRIMARY KEY,
    started_at             TEXT    NOT NULL,
    finished_at            TEXT    NOT NULL,
    duration_ms            INTEGER NOT NULL,
    conversations_scanned  INTEGER NOT NULL,
    conversations_changed  INTEGER NOT NULL,
    messages_total         INTEGER NOT NULL,
    messages_added         INTEGER NOT NULL,
    snapshots_seen         INTEGER NOT NULL,
    skipped_lines          INTEGER NOT NULL,
    errors                 INTEGER NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    body,
    content='messages',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
    INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
`

// schemaV2 layers the unified-contacts model on top of v1. It is safe to run
// against any database at version 1 (the only path that can reach it): the new
// tables are CREATEd, conversations is rebuilt to swap UNIQUE(name) for
// UNIQUE(source, name), and every existing Signal conversation is mapped to a
// fresh contact and identifier so the journal / contacts page see a populated
// world from day one. See docs/adr/0003-dual-source-archive.md.
//
// The runner toggles foreign keys off around the apply (SQLite's recommended
// pattern for rebuilding a referenced table) and back on afterward.
const schemaV2 = `
CREATE TABLE IF NOT EXISTS contacts (
    id           INTEGER PRIMARY KEY,
    display_name TEXT    NOT NULL,
    notes        TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS contact_identifiers (
    id         INTEGER PRIMARY KEY,
    contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    source     TEXT    NOT NULL,
    identifier TEXT    NOT NULL,
    UNIQUE(source, identifier)
);
CREATE INDEX IF NOT EXISTS idx_contact_identifiers_contact ON contact_identifiers(contact_id);

CREATE TABLE conversations_new (
    id         INTEGER PRIMARY KEY,
    source     TEXT    NOT NULL DEFAULT 'signal',
    name       TEXT    NOT NULL,
    contact_id INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
    is_group   INTEGER NOT NULL DEFAULT 0,
    UNIQUE(source, name)
);
INSERT INTO conversations_new (id, source, name, contact_id, is_group)
    SELECT id, 'signal', name, NULL, 0 FROM conversations;
DROP TABLE conversations;
ALTER TABLE conversations_new RENAME TO conversations;

ALTER TABLE messages    ADD COLUMN source TEXT NOT NULL DEFAULT 'signal';
ALTER TABLE ingest_runs ADD COLUMN source TEXT NOT NULL DEFAULT 'signal';

-- Bootstrap one contact per existing conversation. Matching by display_name is
-- safe ONLY here: at v1 conversations.name was UNIQUE, and this migration sees
-- Signal data exclusively, so the name→contact join is unambiguous and the
-- LIMIT 1 never discards a distinct person. DO NOT copy this match-by-name
-- pattern into the iMessage importer (Slice 2.5): once two sources share a
-- display_name it would silently merge two different people. Cross-source
-- linking is a deliberate, user-confirmed action on the contacts page
-- (ADR-0003), never a name-equality heuristic.
INSERT INTO contacts (display_name)
    SELECT name FROM conversations;
UPDATE conversations
   SET contact_id = (
       SELECT c.id FROM contacts c WHERE c.display_name = conversations.name LIMIT 1
   );
INSERT INTO contact_identifiers (contact_id, source, identifier)
    SELECT contact_id, source, name FROM conversations WHERE contact_id IS NOT NULL;
`

// schemaV3 adds the vector embeddings table for semantic search.
//
// Embeddings are keyed by the STABLE message hash (messages.hash), not the
// rowid: ReplaceConversationMessages deletes and re-inserts a conversation's
// rows on every re-ingest (changing rowids but not hashes), so keying by hash
// means unchanged messages keep their embeddings and only new/changed content
// is re-embedded. For that reason there is deliberately NO foreign key to
// messages — a CASCADE would wipe embeddings on every re-ingest. Embeddings for
// truly-deleted messages are harmless orphans that `embed --prune` can reclaim.
//
// vec is a little-endian float32 blob of length dim*4. The primary key is
// (message_hash, model) so embeddings from different models COEXIST: switching
// llm.embed_model (or benchmarking two models) does not overwrite and then have
// to re-embed the whole corpus on every switch — each model's vectors persist
// and a re-run under a previously-used model is a no-op.
const schemaV3 = `
CREATE TABLE IF NOT EXISTS embeddings (
    message_hash TEXT    NOT NULL,
    model        TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    vec          BLOB    NOT NULL,
    PRIMARY KEY (message_hash, model)
);
`

// schemaV4 adds AI-extracted contact facts and the per-conversation cursor that
// makes fact extraction incremental.
//
// contact_facts holds atomic, cited facts ABOUT a contact (e.g. "Has a dog
// named Biscuit", category "personal"), each carrying provenance: the source,
// the stable hash of the supporting message, and that message's timestamp.
// Facts are keyed to contacts(id) (not conversations) so a person whose Signal
// and iMessage threads are merged onto one contact accumulates a single,
// deduplicated fact set. fact_hash is a stable digest of the normalized fact
// text; UNIQUE(contact_id, fact_hash) makes PutFact idempotent, so re-running
// extraction (or processing two merged conversations) never duplicates a fact.
// There is deliberately NO foreign key from source_message_hash to messages:
// like embeddings, re-ingest deletes and re-inserts message rows (new rowids,
// stable hashes), so a CASCADE would wipe facts on every import. A fact whose
// supporting message later vanishes simply renders without a jump link.
//
// fact_state is the incrementality cursor: per conversation, the hash of the
// last message fed to the extractor and the chat model that produced the facts.
// The cursor is stored as a HASH (resolved back to a (ts_unix, id) keyset
// position at run time) rather than a rowid so it survives re-ingest. Recording
// the model means a model change re-scans from the start; dedup keeps that safe.
const schemaV4 = `
CREATE TABLE IF NOT EXISTS contact_facts (
    id                  INTEGER PRIMARY KEY,
    contact_id          INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    fact                TEXT    NOT NULL,
    category            TEXT    NOT NULL,
    fact_hash           TEXT    NOT NULL,
    source              TEXT    NOT NULL,
    source_message_hash TEXT    NOT NULL,
    source_ts           TEXT    NOT NULL,
    source_ts_unix      INTEGER NOT NULL,
    model               TEXT    NOT NULL,
    created_at          TEXT    NOT NULL,
    UNIQUE(contact_id, fact_hash)
);
CREATE INDEX IF NOT EXISTS idx_contact_facts_contact ON contact_facts(contact_id);

CREATE TABLE IF NOT EXISTS fact_state (
    conversation_id   INTEGER PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    last_message_hash TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    facts_added       INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT    NOT NULL
);
`

// schemaV5 adds the per-conversation `pinned` flag that drives the sidebar's
// PINNED section (SPEC-0006 REQ-0006-010). A plain additive ALTER: every
// existing conversation defaults to 0 (unpinned), so the migration is a no-op
// for already-populated databases and idempotent on re-run via the version
// guard. Ordering elsewhere is unchanged — the sidebar template, not the query,
// splits pinned from non-pinned, keeping both sections sorted by recency.
const schemaV5 = `
ALTER TABLE conversations ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
`

// schemaV6 adds the reactions table that captures emoji reactions / iMessage
// tapbacks and renders them as badges on the target message (issue #50).
//
// Like embeddings (schemaV3) and contact_facts (schemaV4), reactions are keyed by
// the STABLE per-source message hash (messages.HashWithSource), NOT a message
// rowid: ReplaceConversationMessages deletes and re-inserts a conversation's rows
// on every re-ingest (rowids change, hashes don't), so there is deliberately NO
// foreign key to messages — a CASCADE would wipe reactions on each import. The FK
// is to conversations(id) instead, which gives the store a cheap per-conversation
// DELETE for the same idempotent full-replace pattern the messages use. source is
// stored alongside the hash so two sources that share a display name (and could
// otherwise collide on the same bare ID) stay distinct, matching how the message
// hash is namespaced. UNIQUE(message_hash, emoji, actor) dedups re-inserts within
// a single replace and makes ingestion idempotent.
const schemaV6 = `
CREATE TABLE IF NOT EXISTS reactions (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    message_hash    TEXT    NOT NULL,
    source          TEXT    NOT NULL,
    emoji           TEXT    NOT NULL,
    actor           TEXT    NOT NULL DEFAULT '',
    UNIQUE(message_hash, emoji, actor)
);
CREATE INDEX IF NOT EXISTS idx_reactions_message_hash ON reactions(message_hash);
`

// schemaV7 denormalizes conversation_id onto attachments and links so
// per-conversation media/link counts never have to walk a conversation's
// messages (SPEC-0008 REQ-0008-003: measured 44–112× faster sidebar counts).
//
// The column is a plain copy of the owning message's conversation_id, NOT a
// foreign key: the existing message_id FK already cascade-deletes these rows
// with their message, and adding an FK retroactively would force a full table
// rebuild for no extra integrity. DEFAULT 0 exists only because SQLite requires
// a default when ALTERing a NOT NULL column onto a populated table; the
// backfill UPDATE below rewrites every row from messages in the same
// transaction, and the ingest write path (ReplaceConversationMessages) stamps
// the column on every insert from then on, so 0 never survives outside a
// corrupt database. The UPDATE ... FROM join form (not a correlated subquery)
// leaves any orphaned row — impossible while the FK holds — at 0 instead of
// failing the migration with a NULL.
const schemaV7 = `
ALTER TABLE attachments ADD COLUMN conversation_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE links       ADD COLUMN conversation_id INTEGER NOT NULL DEFAULT 0;

UPDATE attachments SET conversation_id = m.conversation_id
  FROM messages m WHERE m.id = attachments.message_id;
UPDATE links SET conversation_id = m.conversation_id
  FROM messages m WHERE m.id = links.message_id;

CREATE INDEX IF NOT EXISTS idx_attachments_conv_kind ON attachments(conversation_id, kind);
CREATE INDEX IF NOT EXISTS idx_links_conv            ON links(conversation_id);
`

// schemaV8 denormalizes the owning message's ts_unix onto attachments and
// links so the media gallery can count, filter, and time-order both tables
// without ever touching messages (SPEC-0008 REQ-0008-009; measured on the
// reference archive: unfiltered attachment listing 357 ms → 10 ms, links
// dedup+count 2.07 s → ~35 ms). Same design as v7's conversation_id: a plain
// copy stamped by the ingest write path, backfilled here, with DEFAULT 0 only
// because SQLite requires one when ALTERing a NOT NULL column onto a populated
// table.
//
// idx_attachments_kind_ts serves the gallery's newest-first walk: with the
// implicit rowid suffix, ORDER BY ts_unix DESC, id DESC within a kind is
// exactly a backward index scan — no sort, no messages join.
//
// idx_links_gallery is deliberately covering (url, ts_unix, domain,
// conversation_id, message_id): the links tab deduplicates by URL with
// COUNT(*) + earliest occurrence, and COUNT(DISTINCT url) feeds the tab badge;
// both become pure index scans instead of scattered row fetches across the
// whole table (measured: distinct-URL count 65 ms → 2 ms).
const schemaV8 = `
ALTER TABLE attachments ADD COLUMN ts_unix INTEGER NOT NULL DEFAULT 0;
ALTER TABLE links       ADD COLUMN ts_unix INTEGER NOT NULL DEFAULT 0;

UPDATE attachments SET ts_unix = m.ts_unix FROM messages m WHERE m.id = attachments.message_id;
UPDATE links SET ts_unix = m.ts_unix FROM messages m WHERE m.id = links.message_id;

CREATE INDEX IF NOT EXISTS idx_attachments_kind_ts ON attachments(kind, ts_unix);
CREATE INDEX IF NOT EXISTS idx_links_gallery       ON links(url, ts_unix, domain, conversation_id, message_id);
`

// schemaV9 adds the node-local device-sync state tables (ADR-0018 /
// SPEC-0011 "Database Operation Standards"). Two tables, never synchronized —
// they describe THIS node's view of its paired peers — and completely inert
// on nodes that never enable device sync (created empty, no triggers, no
// changes to existing tables).
//
// paired_devices is the peer registry: one row per paired device, keyed by
// the pinned certificate fingerprint (canonical lowercase-hex SHA-256 of the
// peer's certificate DER, UNIQUE — a fingerprint identifies exactly one
// peer). roles is a JSON object mapping source name → the role the PEER
// plays for that source ("importer" | "replica"); it lives in a JSON column
// rather than a child table because the registry is tiny (a handful of
// devices), is always read whole, and single-importer-per-source enforcement
// (SPEC-0011 "Importer and Replica Roles") happens in one transaction in
// internal/store/devices.go where it can name the conflicting incumbent.
// Deleting a row IS revocation: unpair removes the pin, and the TLS layer
// rejects the certificate from then on.
//
// sync_state carries both halves of the replica's durable sync bookkeeping,
// keyed (peer_id, source, rel_path):
//
//   - rel_path = ” is the per-(peer, source) MANIFEST GENERATION row —
//     `generation` records the last manifest generation fully adopted from
//     that peer.
//   - rel_path <> ” rows are TRANSFER CURSORS: one in-flight or verified
//     file, its manifest-declared size/hash, and the resumable byte offset.
//
// One table for both means "round adoption is atomic in sync state"
// (SPEC-0011 scenario) is a single-transaction UPDATE across rows of the same
// table — a crash can never record a generation as complete with stale
// cursors. Cursor rows cascade with their peer: unpairing severs future sync
// but never touches archive files (SPEC-0011 "Unpairing and Revocation").
const schemaV9 = `
CREATE TABLE IF NOT EXISTS paired_devices (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    fingerprint  TEXT    NOT NULL UNIQUE,
    address      TEXT    NOT NULL,
    roles        TEXT    NOT NULL DEFAULT '{}',
    paired_at    TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sync_state (
    peer_id       INTEGER NOT NULL REFERENCES paired_devices(id) ON DELETE CASCADE,
    source        TEXT    NOT NULL,
    rel_path      TEXT    NOT NULL DEFAULT '',
    generation    INTEGER NOT NULL DEFAULT 0,
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    sha256        TEXT    NOT NULL DEFAULT '',
    fetched_bytes INTEGER NOT NULL DEFAULT 0,
    verified      INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT    NOT NULL,
    PRIMARY KEY (peer_id, source, rel_path)
);
`

// schemaV10 repurposes the device-sync tables for the Syncthing engine
// (ADR-0021 supersedes ADR-0018; SPEC-0014 REQ "Migration from SPEC-0011").
// The SPEC-0011 shapes — a peer registry keyed by pinned msgbrowse
// certificate fingerprint, and per-file byte-range transfer cursors — belong
// to the retired bespoke transport; Syncthing owns identity, transfer, and
// resumption now, so both tables are rebuilt:
//
//   - paired_devices: one row per paired peer, keyed by its UNIQUE Syncthing
//     device ID (the SHA-256 of the peer's TLS certificate — the mutual-TLS
//     identity Syncthing pins). folders is a JSON array of the managed
//     archive folder ids shared with that peer ("msgbrowse-<source>");
//     roles stays a JSON object for the importer/replica role story (#158).
//   - sync_state: one row per managed Syncthing folder — the folder↔source
//     mapping plus the last time a folder-completion event triggered the
//     incremental re-ingest (SPEC-0014 REQ "Re-ingest Trigger"). Byte-level
//     bookkeeping is gone: Syncthing resumes its own transfers.
//
// Existing SPEC-0011 rows are CLEARED, not converted — a pinned certificate
// fingerprint has no Syncthing device-ID equivalent (the peer must be
// re-paired by scanning the new device-ID QR), and SPEC-0014 requires the
// migration to leave "no dangling pinned certificates". DROP+CREATE inside
// the migration transaction does exactly that; both tables remain node-local
// and are never synchronized.
const schemaV10 = `
DROP TABLE IF EXISTS sync_state;
DROP TABLE IF EXISTS paired_devices;

CREATE TABLE paired_devices (
    id           INTEGER PRIMARY KEY,
    device_id    TEXT    NOT NULL UNIQUE,
    name         TEXT    NOT NULL DEFAULT '',
    folders      TEXT    NOT NULL DEFAULT '[]',
    roles        TEXT    NOT NULL DEFAULT '{}',
    paired_at    TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE sync_state (
    folder_id      TEXT    PRIMARY KEY,
    source         TEXT    NOT NULL,
    last_import_at TEXT    NOT NULL DEFAULT '',
    updated_at     TEXT    NOT NULL
);
`

// schemaV12 adds embed_runs — the durable log of semantic-search indexing
// runs, the embeddings analogue of ingest_runs (issue #1: the Overview needs
// "last index run" and an in-progress marker, and internal/embed's
// run-time Summary evaporates with the CLI process that produced it).
//
// A run is recorded in two (plus N) writes: a row is INSERTed when the run
// starts (finished_at = ” — the sentinel for "still in flight"), its
// embedded/batches counters and updated_at heartbeat are refreshed after each
// batch, and the terminal write stamps finished_at plus the final totals (and
// the error text when the run aborted). `msgbrowse embed` is a separate
// process from `msgbrowse serve`, but both open the same SQLite file, so the
// heartbeat is exactly how the web Overview observes a live run: an
// unfinished row with a fresh updated_at is in progress; an unfinished row
// whose heartbeat went stale is a crashed run (the process died before its
// terminal write) and is reported as such rather than spinning forever.
//
// All timestamps are RFC3339 UTC strings, matching ingest_runs.
const schemaV12 = `
CREATE TABLE IF NOT EXISTS embed_runs (
    id          INTEGER PRIMARY KEY,
    model       TEXT    NOT NULL,
    started_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL,
    finished_at TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    embedded    INTEGER NOT NULL DEFAULT 0,
    pruned      INTEGER NOT NULL DEFAULT 0,
    batches     INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT ''
);
`

// schemaV13 adds the cross-provider contact-merge decision journal and its
// rules (ADR-0024 / SPEC-0018, issue #11). Note the version: the ADR draft
// wrote "migration v11", then embed_runs claimed v11 on this lineage, and the
// v11 slot ultimately went to the journal tables that shipped on main (issue
// #217) — so the merge tables land at v13. The tables and behavior are exactly
// as the ADR pins; only the migration number moved.
//
// contact_links is a DECISION JOURNAL, not a pointer graph: one row per
// merge/split decision keyed by a canonical-ordered pair of stable
// (source, identifier) tuples — never contact rowids. This is the codebase's
// standard answer to rowid churn (embeddings v3, contact_facts v4, reactions
// v6 all key durable state by stable message hashes because
// ReplaceConversationMessages / DeleteSourceData reassign rowids on every
// import): (source, identifier) is UNIQUE and stable in contact_identifiers
// since v2, so a decision recorded against it re-activates when its source is
// re-imported. There is deliberately NO foreign key to contact_identifiers —
// a link whose identifier is currently absent is INERT, not invalid, exactly
// like a fact whose supporting message hash has vanished.
//
// Canonical pair ordering ((source_a, identifier_a) < (source_b,
// identifier_b), enforced by the writer, deduped by the UNIQUE constraint)
// makes symmetric decisions collide, and "one current decision per pair" (the
// writer replaces a split row with a merge row and vice versa on manual
// action) keeps the table free of contradictions without timestamp
// arbitration. A merge records the full bipartite pairing of both contacts'
// identifier sets so any surviving pair re-links the group after a partial
// source deletion. origin ('manual' | 'auto') drives precedence in reconcile:
// manual split > manual merge > auto rules.
//
// contact_merge_rules is a single-row settings table (CHECK id = 1) owned by
// the settings UI (#12); it lives in the store, not the config file, because
// the web layer has no config-file write path. Defaults are conservative
// (ADR-0024): auto-merge OFF (suggestions only), phone+email trusted once
// enabled, address-book hints on (they only ever suggest, and the provider is
// permission-gated). The default row is seeded here idempotently.
const schemaV13 = `
CREATE TABLE IF NOT EXISTS contact_links (
    id           INTEGER PRIMARY KEY,
    kind         TEXT    NOT NULL,          -- 'merge' | 'split'
    origin       TEXT    NOT NULL,          -- 'manual' | 'auto'
    source_a     TEXT    NOT NULL,
    identifier_a TEXT    NOT NULL,
    source_b     TEXT    NOT NULL,
    identifier_b TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,
    UNIQUE(source_a, identifier_a, source_b, identifier_b)
);
CREATE INDEX IF NOT EXISTS idx_contact_links_a ON contact_links(source_a, identifier_a);
CREATE INDEX IF NOT EXISTS idx_contact_links_b ON contact_links(source_b, identifier_b);

CREATE TABLE IF NOT EXISTS contact_merge_rules (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    auto_merge       INTEGER NOT NULL DEFAULT 0,
    match_phone      INTEGER NOT NULL DEFAULT 1,
    match_email      INTEGER NOT NULL DEFAULT 1,
    use_address_book INTEGER NOT NULL DEFAULT 1,
    updated_at       TEXT    NOT NULL
);
INSERT INTO contact_merge_rules (id, auto_merge, match_phone, match_email, use_address_book, updated_at)
    VALUES (1, 0, 1, 1, 1, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
    ON CONFLICT(id) DO NOTHING;
`

// schemaV11 adds the AI-editorialized journal (ADR-0023): a two-layer feature
// over the archive, both layers keyed by calendar DAY ('YYYY-MM-DD').
//
// journal_days is the MECHANICAL layer — a deterministic per-day rollup
// (message/conversation counts, per-source counts, top senders) derived purely
// from messages. It is a cache/index: always rebuildable from the source rows,
// so stale entries are harmless and a DELETE+rebuild is cheap. source_counts
// and top_senders are JSON blobs (the shapes are tiny and always read whole).
//
// journal_digests is the LLM layer — one digest per day. It is versioned by
// (model, prompt_version) so switching llm.chat_model or editing
// journal.digest_prompt invalidates the cached digest and re-runs make that day
// eligible again. prompt_version is a sha256 of the normalized effective prompt,
// the same recipe contact_facts uses (internal/store/facts.go factHash).
//
// body holds the plain-text summary (also the empty-response guard + the
// fallback when structured parsing fails). structured is the canonical JSON of
// the editorial digest (summary/people/themes/highlights/standout_media/
// notable_links) — a read-whole blob, like journal_days.source_counts. mood is
// denormalized out of the digest into its own column so the calendar/heatmap can
// tint up to 366 day cells with one bounded range query, never unmarshaling 366
// blobs (the schemaV7/V8 denormalization rationale). Both default ” so a
// pre-structured (prose-only) digest row and a legacy DB read cleanly.
//
// Like embeddings (schemaV3) and contact_facts (schemaV4), NEITHER table has a
// foreign key to messages: ReplaceConversationMessages deletes and re-inserts a
// conversation's message rows on every re-ingest (rowids change, content does
// not), so a CASCADE would wipe every derived journal row on each import. Both
// tables are day-keyed and FK-less, so the migration's foreign_key_check passes
// trivially. Day bucketing is UTC (substr(ts,1,10) == date(ts_unix,'unixepoch'))
// because ts_unix is the wall-clock string parsed AS UTC — any 'localtime'
// conversion would double-shift and misfile messages across day boundaries.
const schemaV11 = `
CREATE TABLE IF NOT EXISTS journal_days (
    day                TEXT    PRIMARY KEY,
    message_count      INTEGER NOT NULL,
    conversation_count INTEGER NOT NULL,
    source_counts      TEXT    NOT NULL DEFAULT '{}',
    top_senders        TEXT    NOT NULL DEFAULT '[]',
    updated_at         TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS journal_digests (
    day            TEXT    PRIMARY KEY,
    model          TEXT    NOT NULL,
    prompt_version TEXT    NOT NULL,
    body           TEXT    NOT NULL,
    structured     TEXT    NOT NULL DEFAULT '',
    mood           TEXT    NOT NULL DEFAULT '',
    updated_at     TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_journal_digests_updated ON journal_digests(updated_at);
`

// schemaV14 is the LINEAGE REPAIR migration (issue #217).
//
// Between 2026-07-07 and 2026-07-24 two lineages independently defined a
// migration numbered v11 and both were applied to real databases:
//
//	main lineage  v11 = journal_days + journal_digests   (shipped in #213)
//	fork lineage  v11 = embed_runs                       (shipped in #29's branch)
//	fork lineage  v12 = contact_links + contact_merge_rules
//
// The reconciliation above gives main's journal tables the v11 slot they
// already occupy in the wild and renumbers the fork's pair to v12/v13. That
// alone is NOT sufficient, because migrate() only ever runs versions
// current+1 … schemaVersion: a fork-lineage database stamped 11 or 12 is
// already "past" v11 and would never receive the journal tables, while a
// main-lineage database stamped 11 would never receive embed_runs. Both would
// arrive at v13 reporting a schema they do not actually have.
//
// v14 closes that hole by re-asserting the union of all three contested
// migrations. Every database converges here regardless of the path it took:
//
//	v10 (common ancestor) → v11, v12, v13 applied normally, v14 a no-op
//	main lineage @ v11    → v12, v13 create the fork tables, v14 a no-op
//	fork lineage @ v11    → v12 no-op, v13 contacts, v14 creates the journal
//	fork lineage @ v12    → v13 no-op, v14 creates the journal
//
// This is safe to re-run because all three constants are written to be
// idempotent within their version transition (the invariant documented on
// `migrations` above): every statement is CREATE … IF NOT EXISTS, and the one
// seed row uses ON CONFLICT DO NOTHING. It is composed from the constants
// rather than copied so it can never drift from what it repairs — which holds
// precisely because shipped migrations are immutable (enforced by
// scripts/check-migrations.sh).
//
// Databases created after this migration never exercise the repair path; they
// walk 1…14 in order and v14 is a no-op on a schema that is already complete.
const schemaV14 = schemaV11 + schemaV12 + schemaV13

// schemaV15 adds what in-app journal building needs to be observable (#240).
//
//   - journal_digests.message_count records the day's mechanical message count
//     AT THE TIME the digest was written. A digest written before more messages
//     landed on that day is stale, and without this there is nothing to compare
//     against. Legacy rows default to 0, which means UNKNOWN, NOT "zero
//     messages" — every staleness test must require `> 0` explicitly, or every
//     pre-v15 digest would read as stale the moment this ships.
//   - journal_runs is the durable log of journal passes: the journal analogue of
//     embed_runs (v12). Without it there is nowhere for "last run", "duration",
//     "digests so far", or "interrupted" to come from. It is also the only
//     channel between a `msgbrowse journal` CLI process and a running
//     `msgbrowse serve` sharing the same SQLite file, which is what lets the
//     page refuse to start a second run rather than race one already in flight.
//
// scope is ” for a whole-archive pass, else the single day a per-day Rebuild
// targeted. Timestamps are RFC3339 UTC strings, matching embed_runs and
// ingest_runs.
//
// A plain additive ALTER with a DEFAULT (SQLite requires one when adding a NOT
// NULL column to a populated table); precedent: schemaV5, schemaV8. Deliberately
// NOT folded into schemaV11 (which owns journal_digests) — shipped migrations
// are immutable, and CREATE TABLE IF NOT EXISTS would skip an existing table
// anyway, so the column would silently never appear.
const schemaV15 = `
ALTER TABLE journal_digests ADD COLUMN message_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS journal_runs (
    id          INTEGER PRIMARY KEY,
    model       TEXT    NOT NULL,
    scope       TEXT    NOT NULL DEFAULT '',
    started_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL,
    finished_at TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    days        INTEGER NOT NULL DEFAULT 0,
    digested    INTEGER NOT NULL DEFAULT 0,
    cached      INTEGER NOT NULL DEFAULT 0,
    skipped     INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT ''
);
`

// schemaV16 backfills the 'video' attachment kind for existing rows whose file
// extension is a known video format. Before this, all non-image attachments were
// stamped 'file'; the Movies gallery tab (issue #4) needs them reclassified so
// they appear under Videos instead of Files.
const schemaV16 = `
UPDATE attachments
   SET kind = 'video'
 WHERE kind = 'file'
   AND (
       lower(rel_path) LIKE '%.mp4'
    OR lower(rel_path) LIKE '%.mov'
    OR lower(rel_path) LIKE '%.avi'
    OR lower(rel_path) LIKE '%.webm'
    OR lower(rel_path) LIKE '%.mkv'
    OR lower(rel_path) LIKE '%.m4v'
    OR lower(rel_path) LIKE '%.mpg'
    OR lower(rel_path) LIKE '%.mpeg'
    OR lower(rel_path) LIKE '%.3gp'
   );
`

// schemaV17 lays down IPIP-anchored sentiment scoring (ADR-0028, SPEC-0027).
//
// message_sentiment is a derived cache in the ADR-0002 sense and therefore has
// NO foreign key to messages: it is keyed by the message's content hash, so a
// re-ingest that deletes and re-inserts message rows with new rowids leaves the
// scores intact and still joinable. It is also sparse — only constructs the
// model reported as salient for a message get a row — and stamped with the
// (model, lexicon_version) generation that produced it, because scores from
// different models or curations are not comparable and must never be averaged
// together. The UNIQUE constraint is what makes writes idempotent upserts, so
// rescanning a conversation is always safe.
//
// contact_sentiment_optout is deliberately FK-less too, for a different reason:
// it is a privacy preference, not derived data. Letting an ON DELETE CASCADE
// drop it would silently re-enable scoring for a contact whose row was rewritten
// by a merge/split (SPEC-0018), which is the one direction this table must never
// fail in. A dangling marker is harmless; a vanished one is not.
const schemaV17 = `
CREATE TABLE IF NOT EXISTS message_sentiment (
    message_hash    TEXT    NOT NULL,
    model           TEXT    NOT NULL,
    lexicon_version TEXT    NOT NULL,
    construct       TEXT    NOT NULL,
    score           REAL    NOT NULL,
    ts_unix         INTEGER NOT NULL,
    contact_id      INTEGER NOT NULL,
    UNIQUE(message_hash, model, lexicon_version, construct)
);

-- The read-side aggregates (profile sentiment-over-time, trait sketch) always
-- filter to one contact and the current generation, then bucket by time.
CREATE INDEX IF NOT EXISTS idx_message_sentiment_contact
    ON message_sentiment(contact_id, model, lexicon_version, ts_unix);

-- The journal mood strip buckets every contact's scores by UTC day.
CREATE INDEX IF NOT EXISTS idx_message_sentiment_ts
    ON message_sentiment(model, lexicon_version, ts_unix);

CREATE TABLE IF NOT EXISTS sentiment_state (
    conversation_id INTEGER PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    last_message_hash TEXT  NOT NULL,
    model           TEXT    NOT NULL,
    lexicon_version TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS contact_sentiment_optout (
    contact_id INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL
);
`

// schemaV18 lays down the unsolicited-contact (spam) evidence layer
// (ADR-0029, SPEC-0028).
//
// Every table here is a DERIVED CACHE over the archive in the ADR-0002 sense,
// with one deliberate exception (spam_senders, below), so none of them carry a
// foreign key to messages: a re-ingest deletes and re-inserts message rows with
// new rowids but identical content hashes, and an ON DELETE CASCADE would wipe
// the evidence record on every import. Keying by message hash is what lets a
// dossier assembled today still resolve after tomorrow's re-export.
//
// spam_findings is the per-message scan result: one row per message the ruleset
// examined, stamped with the ruleset_version that produced it, because findings
// from two different rule configurations are not comparable and must never be
// mixed in one dossier. It is NOT sparse — a non-matching message from a
// stranger still gets a row with empty reasons, because "we looked at this
// message and it tripped nothing" is itself evidence, and the trailing-12-month
// counts are counts of contact, not counts of violations.
//
// spam_senders is the one table that is NOT purely derived: status,
// suspected_entity, consent_status, consent_notes and notes are HUMAN
// judgments, entered by hand and never overwritten by a scan. A scan may
// promote 'seen' to 'watch'; it may not touch anything a person set. That is
// why the table survives `spam scan --reset`, which clears only the derived
// halves (findings, cursors, and scan-origin events).
//
// spam_events records things that happened outside the message stream (an FCC
// complaint, a 7726 report) alongside the opt-outs detected inside it. origin
// distinguishes the two: a detected STOP ('scan') is re-derivable, a typed one
// ('manual') is not, and only the former is cleared by --reset. The UNIQUE
// constraint makes both insert paths idempotent.
//
// spam_state is the per-conversation incremental cursor, the same
// hash-anchored keyset design as fact_state and sentiment_state: a stored
// ruleset_version different from the current one rescans the conversation from
// the top, which is safe because every write is an idempotent upsert.
const schemaV18 = `
CREATE TABLE IF NOT EXISTS spam_senders (
    id                INTEGER PRIMARY KEY,
    source            TEXT    NOT NULL,
    identifier        TEXT    NOT NULL,
    conversation_name TEXT    NOT NULL DEFAULT '',
    status            TEXT    NOT NULL DEFAULT 'seen',
    suspected_entity  TEXT    NOT NULL DEFAULT '',
    consent_status    TEXT    NOT NULL DEFAULT 'no_consent_on_record',
    consent_notes     TEXT    NOT NULL DEFAULT '',
    notes             TEXT    NOT NULL DEFAULT '',
    first_seen_unix   INTEGER NOT NULL DEFAULT 0,
    last_seen_unix    INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL,
    UNIQUE(source, identifier)
);

CREATE TABLE IF NOT EXISTS spam_findings (
    message_hash    TEXT    NOT NULL,
    ruleset_version TEXT    NOT NULL,
    source          TEXT    NOT NULL,
    identifier      TEXT    NOT NULL,
    direction       TEXT    NOT NULL,
    ts_unix         INTEGER NOT NULL,
    reasons         TEXT    NOT NULL DEFAULT '[]',
    urls            TEXT    NOT NULL DEFAULT '[]',
    phones          TEXT    NOT NULL DEFAULT '[]',
    emails          TEXT    NOT NULL DEFAULT '[]',
    names_matched   TEXT    NOT NULL DEFAULT '[]',
    entities        TEXT    NOT NULL DEFAULT '[]',
    is_candidate    INTEGER NOT NULL DEFAULT 0,
    is_after_optout INTEGER NOT NULL DEFAULT 0,
    UNIQUE(message_hash, ruleset_version)
);

-- Every read side (sender stats, violations, dossier, the rolling-window math)
-- filters to one sender and one ruleset generation, then orders by time.
CREATE INDEX IF NOT EXISTS idx_spam_findings_sender
    ON spam_findings(source, identifier, ruleset_version, ts_unix);

CREATE TABLE IF NOT EXISTS spam_events (
    id            INTEGER PRIMARY KEY,
    source        TEXT    NOT NULL,
    identifier    TEXT    NOT NULL,
    event_type    TEXT    NOT NULL,
    event_at      TEXT    NOT NULL,
    event_at_unix INTEGER NOT NULL,
    details       TEXT    NOT NULL DEFAULT '',
    origin        TEXT    NOT NULL DEFAULT 'manual',
    message_hash  TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL,
    UNIQUE(source, identifier, event_type, event_at_unix, message_hash)
);

CREATE INDEX IF NOT EXISTS idx_spam_events_sender
    ON spam_events(source, identifier, event_at_unix);

CREATE TABLE IF NOT EXISTS spam_state (
    conversation_id   INTEGER PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    last_message_hash TEXT    NOT NULL,
    ruleset_version   TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL
);
`

// schemaV19 adds the display-name merge rule (issue #363).
//
// Some sources carry no real handle at all: a signal-export archive identifies
// a conversation only by the directory it lives in, which is the counterparty's
// profile name, and nothing in chat.md carries a phone number or an account
// UUID. On a real archive that left 0 of 115 Signal identifiers phone- or
// email-shaped, so ReasonPhone and ReasonEmail could never fire and not one
// automatic merge had ever happened. Display-name equivalence is the only
// evidence available for those contacts.
//
// It defaults ON because it is suggest-only — the reconcile pass filters
// ReasonDisplayName out of auto-merge exactly as it filters address-book hints,
// so turning it on adds review-queue entries and can never merge anyone by
// itself. Merging two people who happen to share a name would silently blend
// two archives, which is the one failure this feature must not have.
//
// The identifier REPAIR is deliberately not here. Re-deriving an identifier
// needs the same Normalize/DeriveIdentity logic the importers use, which is Go,
// not SQL; expressing it as a SQL heuristic would be a second, subtly different
// implementation of the rule. RepairContactIdentities does it instead and runs
// idempotently on every import, so an existing 2,429-contact archive heals on
// its next sync rather than needing a reimport.
const schemaV19 = `
ALTER TABLE contact_merge_rules ADD COLUMN match_display_name INTEGER NOT NULL DEFAULT 1;
`

// Governing: ADR-0029 (unsolicited-contact evidence) §2
// Implements: SPEC-0028 REQ-0028-013 "Scan-environment provenance", SPEC-0028 REQ-0028-004 "Schema — hash-keyed, FK-less, re-ingest-safe"
//
// schemaV20 stamps the scan ENVIRONMENT onto the evidence layer (issue #385).
//
// ruleset_version digests the rules. It does not digest the conditions the
// rules ran under, and one of those changes what a scan concludes: whether an
// address book was readable. A desktop scan (Contacts wired via
// SetContactResolver) and a release-CLI scan (CGO_ENABLED=0, no provider) use
// two different stranger predicates — address-book membership versus the
// phone/email-shaped-thread heuristic — and before this migration both wrote
// into the same generation with nothing recording which produced a row.
//
// The stamp is "provider/availability" (spam.scanEnv): the resolver's identity
// and what it could actually do. The degraded flag REQ-0028-013 also names is
// not a third column because it is fully determined by the availability half
// (degraded <=> it is not "available", which is the branch in spam.Run);
// storing it too would be denormalized and could disagree with itself.
//
// This is deliberately NOT folded into ruleset_version. A degraded scan is not
// a different ruleset, it is the same rules over a weaker input, and hashing
// availability into the version would re-derive the entire layer every time you
// switch between the desktop app and the CLI. Recording it keeps both facts —
// the rules that ran, and what they could see — without that cost.
//
// ” is the honest default for rows that predate this migration: they were
// produced under an environment nobody recorded, which is distinct from all
// three known values and is reported as unknown provenance rather than guessed.
const schemaV20 = `
ALTER TABLE spam_findings ADD COLUMN scan_env TEXT NOT NULL DEFAULT '';
ALTER TABLE spam_state    ADD COLUMN scan_env TEXT NOT NULL DEFAULT '';
`
