-- The two tables artefactd owns: the artefact record, and the disclosures
-- made against it.
--
-- One artefact row is everything about a generated document except the
-- document: where it is, who made it, what it is classified at, what it is
-- sealed with, and what happened to it. The payload is in S3 and nothing here
-- ever holds a byte of it.
--
-- THE ROW OUTLIVES THE KEY. Ruling 6 makes erasure work by destroying the
-- wrapped key while the record that the artefact existed — and who was given
-- access to it — survives, because an audit trail must keep the fact and lose
-- the content. So there is no DELETE anywhere in this service's queries: a
-- shredded artefact is `state = 'SHREDDED'`, `wrapped_dek IS NULL` and a
-- `shredded_at`, and the disclosures against it stay readable.

CREATE TABLE IF NOT EXISTS artefacts (
    -- A ULID, lowercase. It sorts by time as a string and it is the last
    -- component of the object key.
    id                   text PRIMARY KEY,

    -- The tenant and the run come from Garm-Invocation on the call that
    -- named the artefact, and are never request fields. They are on the row
    -- because they are the first two components of the key and because an
    -- artefact outlives the call that created it.
    tenant               text        NOT NULL,
    run_id               text        NOT NULL,

    -- <tenant>/<run>/<artefact_id>, built by one function with a property
    -- test over it. Unique, because two rows resolving to one object is the
    -- shape a path-construction bug takes when it does not cross a tenant.
    object_key           text        NOT NULL UNIQUE,

    state                text        NOT NULL,

    -- STAMPED, NEVER SUPPLIED. The classification comes from the creating
    -- run's principal; no request message in this contract has a field for
    -- it, which is asserted over the descriptor rather than checked at run
    -- time.
    clearance            text        NOT NULL,
    compartments         text[]      NOT NULL DEFAULT '{}',

    -- What the bytes are, and what to call them in front of a person.
    media_type           text        NOT NULL,
    label                text        NOT NULL DEFAULT '',

    -- Provenance. The tool that generated it, the principal it was generated
    -- for, and the daemon's own ledger event id for each of the two calls —
    -- the call id garmd sets on the invocation IS that row's id, so a commit
    -- can be chased back to the ledger without this service writing to it.
    tool_fqn             text        NOT NULL,
    created_by           text        NOT NULL,
    begin_call_id        text        NOT NULL DEFAULT '',
    commit_call_id       text        NOT NULL DEFAULT '',

    -- Crypto, and the parts that are forgotten until a rotation is needed.
    --
    -- `codec` names the algorithm this artefact's payload was sealed with, so
    -- a future codec can still open an old object rather than the store being
    -- stuck on its first choice for ever. `kek_id` names the wrapping key and
    -- its version, so a KEK rotation is expressible even though the KMS
    -- integration is out of scope. There is no nonce column: a codec that
    -- needs one writes it into the payload it seals, and says so in its own
    -- documentation.
    --
    -- `wrapped_dek` is NULL only when the key has been DESTROYED. The null
    -- codec stores a zero-length value rather than NULL, so the distinction
    -- between "there was never a key" and "the key is gone" survives.
    codec                text        NOT NULL,
    kek_id               text        NOT NULL DEFAULT '',
    wrapped_dek          bytea,

    -- Two digests and two sizes, named apart on purpose. The plaintext pair
    -- is what a reader verifies once it has opened the payload; the stored
    -- pair is what the object store holds, which is the sealed payload.
    -- Calling both of them `sha256` is how somebody later compares the wrong
    -- pair.
    plaintext_sha256     text        NOT NULL DEFAULT '',
    plaintext_size_bytes bigint      NOT NULL DEFAULT 0,
    stored_sha256        text        NOT NULL DEFAULT '',
    stored_size_bytes    bigint      NOT NULL DEFAULT 0,

    -- RETENTION is the artefact's lifetime, declared by the creating tool.
    -- It is not the presigned URL's expiry, which is per mint and is stored
    -- nowhere: one is measured in years and the other in minutes, and the
    -- two are named apart here so nobody conflates them.
    retention_seconds    bigint      NOT NULL,

    created_at           timestamptz NOT NULL,
    -- How long the presigned PUT this row was created with is good for. A row
    -- still PENDING past it is an orphan and the sweeper removes it.
    upload_expires_at    timestamptz NOT NULL,
    committed_at         timestamptz,
    retain_until         timestamptz,
    shredded_at          timestamptz,

    CONSTRAINT artefacts_state_check CHECK (state IN ('PENDING', 'COMMITTED', 'SHREDDED')),
    -- A shredded artefact has no key and says when it lost it. This is the
    -- constraint that makes crypto-shredding a fact about the row rather
    -- than a convention in Go.
    CONSTRAINT artefacts_shredded_check CHECK (
        state <> 'SHREDDED' OR (wrapped_dek IS NULL AND shredded_at IS NOT NULL)
    ),
    -- A committed artefact knows when, what it holds and for how long.
    CONSTRAINT artefacts_committed_check CHECK (
        state <> 'COMMITTED'
        OR (committed_at IS NOT NULL AND retain_until IS NOT NULL AND plaintext_sha256 <> '')
    )
);

-- The sweeper's two questions: which pending rows have outlived their upload
-- window, and which committed rows have outlived their retention.
CREATE INDEX IF NOT EXISTS artefacts_pending_idx
    ON artefacts (upload_expires_at) WHERE state = 'PENDING';
CREATE INDEX IF NOT EXISTS artefacts_retain_idx
    ON artefacts (retain_until) WHERE state = 'COMMITTED';
CREATE INDEX IF NOT EXISTS artefacts_tenant_run_idx ON artefacts (tenant, run_id);

-- Every presigned read URL this service ever minted, and who got it.
--
-- The row is written BEFORE the URL exists and in the same transaction that
-- reads the key, so a disclosure that cannot be recorded is a disclosure that
-- does not happen. That is the whole reason minting is a governed call rather
-- than a library function.
--
-- `clearance` and `compartments` here are what the card was LABELLED at, not
-- the caller's — this service is never told a caller's clearance and must not
-- be. An auditor joining this row to the daemon's own ledger row for the same
-- `call_id` can see both what was offered and whether the daemon withheld it.
CREATE TABLE IF NOT EXISTS disclosures (
    seq            bigserial   PRIMARY KEY,
    artefact_id    text        NOT NULL REFERENCES artefacts(id),
    at             timestamptz NOT NULL,
    tenant         text        NOT NULL,
    subject        text        NOT NULL,
    -- The daemon's ledger event id for the call that asked.
    call_id        text        NOT NULL,
    clearance      text        NOT NULL,
    compartments   text[]      NOT NULL DEFAULT '{}',
    url_expires_at timestamptz NOT NULL,
    -- Whether a data key was released with the URL. False under the null
    -- codec, which is what a development plane runs.
    key_released   boolean     NOT NULL
);

CREATE INDEX IF NOT EXISTS disclosures_artefact_idx ON disclosures (artefact_id, seq DESC);
