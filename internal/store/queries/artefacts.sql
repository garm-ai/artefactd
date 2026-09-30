-- Every query this service runs, named, checked against the schema the
-- migrations produce, and STATIC.
--
-- Nothing here is built at run time. Values can be parameterised; identifiers
-- cannot — no SQL dialect binds a column name, a table name or a sort
-- direction — so a builder that takes `OrderBy(req.SortBy)` is not safer than
-- concatenation, it only looks it. The combinations this service needs are
-- small and closed, so they are enumerated as named queries and there is no
-- injection surface at all rather than a guarded one. `TestEveryQueryIsAConstant`
-- asserts that nothing in this package assembles SQL.

-- name: InsertPending :one
INSERT INTO artefacts (
    id, tenant, run_id, object_key, state,
    clearance, compartments, media_type, label,
    tool_fqn, created_by, begin_call_id,
    codec, kek_id, wrapped_dek,
    retention_seconds, created_at, upload_expires_at
) VALUES (
    $1, $2, $3, $4, 'PENDING',
    $5, $6, $7, $8,
    $9, $10, $11,
    $12, $13, $14,
    $15, $16, $17
)
RETURNING *;

-- name: GetArtefact :one
-- By id AND tenant, always. A row of another tenant answers no rows, which
-- the service reports as "no such artefact" — the same answer an id that
-- never existed gets, so a caller cannot map out another tenant's ids.
SELECT * FROM artefacts WHERE id = $1 AND tenant = $2;

-- name: CommitArtefact :one
-- Conditional on the state it requires, so a commit racing a sweep either
-- wins or changes no rows and says so. A re-commit of an already committed
-- artefact matches nothing here; the service reads the row back and treats an
-- identical digest as the same commit.
UPDATE artefacts SET
    state = 'COMMITTED',
    plaintext_sha256 = $3,
    plaintext_size_bytes = $4,
    stored_sha256 = $5,
    stored_size_bytes = $6,
    commit_call_id = $7,
    committed_at = $8,
    retain_until = $9
WHERE id = $1 AND tenant = $2 AND state = 'PENDING'
RETURNING *;

-- name: ShredArtefact :one
-- Crypto-shredding: the key goes, the row stays. `wrapped_dek = NULL` is what
-- makes the payload unreadable under a real codec; the object is deleted
-- separately, which is what makes it unreadable under the null one.
UPDATE artefacts SET
    state = 'SHREDDED',
    wrapped_dek = NULL,
    shredded_at = $2
WHERE id = $1 AND state <> 'SHREDDED'
RETURNING *;

-- name: DeletePending :exec
-- The only DELETE in this service, and it removes a row that was never
-- committed: an artefact nobody confirmed bytes for is a name and nothing
-- else, so there is no fact for an audit trail to keep.
DELETE FROM artefacts WHERE id = $1 AND state = 'PENDING';

-- name: ExpiredPending :many
-- Orphans: a begin_write whose presigned PUT has lapsed with no commit.
SELECT * FROM artefacts
WHERE state = 'PENDING' AND upload_expires_at < $1
ORDER BY upload_expires_at
LIMIT $2;

-- name: ExpiredRetention :many
-- Artefacts that have outlived the retention their creating tool declared.
SELECT * FROM artefacts
WHERE state = 'COMMITTED' AND retain_until < $1
ORDER BY retain_until
LIMIT $2;

-- name: InsertDisclosure :one
-- Written before the URL exists. A disclosure that cannot be recorded is a
-- URL that is not minted.
INSERT INTO disclosures (
    artefact_id, at, tenant, subject, call_id,
    clearance, compartments, url_expires_at, key_released
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: DisclosuresOf :many
SELECT * FROM disclosures WHERE artefact_id = $1 ORDER BY seq;
