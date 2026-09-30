# artefactd — the artefact store

Where a generated document goes, served as a governed tool. Four methods over
one S3 bucket, with this service's own Postgres, reached through `garmd` like
every other tool service.

## The five rules

**1. This service never handles a byte of an artefact.** `begin_write` signs a
PUT and `read_url` signs a GET; the generator and the reader move the bytes
themselves. `internal/blob.Store` deliberately has no `Get` and no `Put`, and a
method that could would be the first step back towards bytes on the request bus.
The descriptor test is the other half of it: no message in `garm.artefacts.v1`
can carry a payload, asserted over the service descriptor rather than
remembered.

**2. The model is given an id, never a URL — and the enforcement is a CHECK.**
`read_url` refuses a caller arriving with a delegation chain or with a principal
that is not a person. It also declares `audience: [AUDIENCE_PERSON]`, and that
is the weaker half: an audience is a **listing** rule and the daemon's
visibility gate never reads it. A tool relying on its audience to keep a caller
out has no gate at all. That mistake cost a day on `garm.tasks.v1`, where
`decide_task` and `approval_card` declared an audience and no tool set.

**3. WHICH artefact a viewer may read is the card's answer, not this service's.**
The invocation carries no clearance and must not — a tool that could see one
would start filtering on it, which is a second unreviewed copy of the policy. So
`read_url` returns a `garm.card.v1.Card` whose label is the artefact's own
classification, and the daemon walks the card's *value* at step 8 and clears it
for a viewer who does not reach the label. Every method here is `CLEARANCE_PUBLIC`
at the method gate for exactly the reason `garm.tasks.v1` is: a method-level
clearance would have to be the lowest any artefact in the deployment carries,
which is no gate.

**4. A URL is not minted without a row.** `read_url` writes the disclosure
before it signs anything, and a row it cannot write is a URL it does not mint.
The order of those two statements in `internal/artefacts/service.go` **is** the
enforcement; a test drops the `disclosures` table and asserts that nothing was
signed.

**5. The row outlives the key.** Erasure destroys the wrapped key and keeps the
record that the artefact existed and who was given access to it. There is one
`DELETE` in this service and it removes a row nobody ever committed bytes for. A
database `CHECK` holds it: a `SHREDDED` row has a `NULL` `wrapped_dek` and a
`shredded_at`, so shredding is a fact about the row and not a convention in Go.

## Layout

```
serve.go              package artefactd: Config and Serve — everything the
                      binary does, as a function a development stack can call
cmd/artefactd/        the binary: flag parsing and a call to Serve
proto/                garm.artefacts.v1 — this repository owns its contract
third_party/proto/    the garm annotations, vendored, never generated
gen/                  generated Go, committed, checked for drift
internal/
  artefacts/          the service — the checks, the card, the handlers
  store/              Postgres: artefacts, disclosures, migrations, sqlc
  blob/               the object key and the presigned URLs
  codec/              the seal on a payload, as an interface with a null impl
  ulid/               the artefact id, which is also a key component
```

`Serve` is the tested entry point: the wire-level test starts the service
through it rather than assembling one by hand, so the path a stack uses is the
path that is covered.

## Where the checks are

`internal/artefacts/service.go`. Every rule about who may do what is in that one
file, in a method per tool; `handlers.go` translates protobuf and nothing else.
A second transport would not be a second set of rules.

Two of them are not there, and both are deliberate. The key construction is in
`internal/blob/key.go`, because it is the one failure that could cross a tenant
and it earns its own package and its own property test. And the classification
check is not here at all: it is the card's label, projected by the daemon.

## sqlc, and why this repository is the first to use it

**`sqlc` is new to this platform and this is its first use.** `tasksd` and
`agentd` both use `pgx` with hand-written SQL in string literals, and neither has
an `sqlc.yaml`. So this is a deliberate introduction rather than following a
pattern, recorded here so it is either adopted platform-wide later or reverted,
rather than left as an island nobody can explain.

**The reasoning.** sqlc gives compile-time checking of SQL against the schema,
which is the same bargain this platform takes everywhere else: declare the thing
and let a check catch the drift. A renamed column becomes a build failure instead
of a query failing in front of a person. That matters more than usual for a store
holding classifications and wrapped key material.

**The schema it generates against IS the migrations** — `sqlc.yaml` points at
`internal/store/migrations`, not at a separately maintained schema file. That is
the whole point: a query and a migration that disagree fail at
`mise run gen-check` rather than at run time. A second schema file would be a
second thing to keep in step, which is the drift this removes.

**How it went, honestly.** It fit. Every query is by id, by tenant or by state —
a small closed set — so there was nothing dynamic to fight, which is the case
where sqlc is known to be awkward. Two frictions worth knowing about for whoever
copies this:

- A `go_type` override for `text[]` must be written as the mapping form
  (`type: string` plus `slice: true`); the string form `"[]string"` is rejected
  as "not a Go basic type", with no hint that the mapping form exists.
- `emit_pointers_for_null_types` does not apply to `timestamptz`, which comes
  back as `pgtype.Timestamptz` regardless. `internal/store/artefacts.go` converts
  at the package boundary so nothing above it sees a driver type.

**No query is assembled, and a test asserts it.** Values can be parameterised;
identifiers cannot — no dialect binds a column name or a sort direction — so a
builder that takes `OrderBy(req.SortBy)` is not safer than concatenation, it only
looks it. This service avoids the question rather than guarding it, and
`TestNoQueryIsAssembled` fails on a format string or a concatenation that reads
like SQL. If a future release genuinely needs dynamism, the rule is: map the
caller's token to a constant with a `switch` that refuses the default case, in
one function, with a test that an unknown token is refused.

**Migrations are `tasksd`'s mechanism, to the line.** Numbered `.sql` files, one
transaction each, recorded in a version table from the first release, applied by
a runner copied from that repository. Consistency in *how* migrations run matters
more than consistency in how queries are written, and a migration framework for
one repository would be a third pattern.

## This repository owns its contract

`garm.artefacts.v1` is here, in `proto/`, the way `garm-ai/tools/web` owns
`web.v1`. Generated Go is **committed**, because a Go module has to build from
its own source — a consumer runs `go build`, not buf plus two plugins plus sqlc —
and `mise run gen-check` is what pays for that.

The garm annotations under `third_party/proto` are vendored verbatim and
`mise run vendor-check` compares them byte for byte against the `contracts`
version **read from `go.mod`**, not against a constant in the task. A constant
would only ever check the tree against itself, which is how a sibling
repository's vendored copies sat five releases behind the module they compiled
against without one red run.

**Nothing may import `github.com/garm-ai/garm`**, and not only for tidiness:
that module and `github.com/garm-ai/contracts` register the same protobuf
descriptor file paths, so a binary linking both builds and then dies in
`protoregistry` at init. `mise run no-old-contracts` fails the build when
`go list -deps` names it, anchored on the module path so `garm-ai/garmd` is
untouched, and reading both the shipped graph and the `-test` one because a test
binary that panics in init is as dead as a shipped one.

## The presigned read window is a trade, not a constant

`--read-url-ttl` defaults to five minutes and is logged at startup. Short is what
makes a leaked link a dead link; the other side is that while this service is
down no NEW read can be minted, and that an outage revokes nothing already
issued. Both halves are in README.md, because an operator choosing the number
needs both.

## Working here

```
mise install    the toolchain
mise run pg     a throwaway Postgres, and the POSTGRES_DSN to export
mise run s3     the environment for the round trip against a real object store
mise run test   go test ./... -race
mise run ci     what CI runs
```

The round trip against a real object store is **not** in `ci`: CI has no object
store, and a test that quietly passed because it skipped would be worse than one
run deliberately.

## The design record is not in this repository

It lives in **[`garm-ai/spec`](https://github.com/garm-ai/spec)** (private),
checked out beside this one at `../spec/docs/superpowers/`.

The one that governs this repository:

- `decisions/2026-09-29-artefacts-live-in-s3-behind-a-governed-service.md` — six
  rulings: one store and it is S3; begin, upload, commit; tenancy is a prefix;
  the model gets an id and never a URL; retention is declared by the creating
  tool; the payload is sealed by a codec and the daemon is not involved.
- `decisions/2026-09-29-the-model-never-sets-control-fields.md` — why the
  classification is stamped and there is nowhere to supply one.
- `specs/2026-09-24-call-stack-design.md` — the ten steps a call goes through
  before it reaches here, and step 8, which is where the card is projected.
- `specs/2026-09-29-cards-and-tasks-as-tools-design.md` — what a card is and how
  its labels are read.

**Do not create `docs/superpowers/` here.**

## This repository is public

No customer, deployment, tenant or internal hostname appears anywhere in it.
Fixtures use `example.com` shapes and generic subjects.
