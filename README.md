# artefactd

**The artefact store.** Where a generated document goes, and what has to happen
before anybody is given a way to read it. Four governed tools over one S3
bucket, reached through `garmd` like any other tool service, with its own
Postgres.

A tool that generates a document cannot return it. The daemon projects an
answer field by field against the caller's clearance and it cannot see inside
an `.xlsx`, so a spreadsheet is disclosed whole or not at all; and the bytes
cannot travel through a tool call either, because they would cross the request
bus everything else depends on. So a generated document needs a store, and
every question about that store turns out to be a governance question.

## The four tools

| | |
|---|---|
| `garm.artefacts.v1.begin_write` | Name an artefact. Returns its id and a presigned PUT. The store chooses the key, stamps the tenant and the classification, and never handles a byte. |
| `garm.artefacts.v1.commit` | Record the digest and the size once the bytes are written. Checks with the object store that something of that size is actually there. |
| `garm.artefacts.v1.describe` | What an artefact is: type, size, label, classification, state. No URL and no key — the response type has nowhere to put either. |
| `garm.artefacts.v1.read_url` | Mint a short-lived URL for one artefact and release its key, for a **person**, recorded as a disclosure before the URL exists. |

`describe` is a method of its own rather than part of `read_url` for three
reasons. A card needs a label, a media type and a size to render, and rendering
discloses nothing. `read_url` writes a disclosure row and mints a credential,
so answering "does this exist and what is it" through it would make the audit
trail claim a disclosure that never happened. And the model's legitimate
question — is it there, what is it, how big — has an answer it is safe to give,
which is this one.

There is no `list`. A generator hands its caller an id; nothing in this release
browses. See `KNOWN-GAPS.md`.

## The write path

```
begin_write  ─▶  id + presigned PUT
                        │
     the generator writes the bytes to S3 itself
                        │
commit       ─▶  digest, size, retain_until
```

One service owns the key namespace, the tenancy prefix and the classification,
and never handles a byte. The rejected alternative — the generator computing
its own key and registering afterwards — puts the key layout and the tenancy
rules in every generator binary. A shared library was rejected for a sharper
reason: minting a read URL is a disclosure, and a library call leaves no ledger
row.

The cost is accepted: a `begin_write` with no `commit` is an orphan, and the
sweeper removes it.

## The model is given an id, never a URL

A presigned URL is a bearer credential — unlimited uses within its lifetime,
from anywhere, recorded nowhere. So a generator returns an `artefact_id`, and
that is what goes in a model's context, in a card, and on a ledger row.

Two things enforce it, and they are different things.

**`read_url` refuses an automated caller.** A call arriving with a delegation
chain is refused, and so is a principal that is not a person: a model's tool
call always carries the chain, and a service calling for itself carries the
kind. It is the mirror image of `garm.tasks.v1.decide_task`'s refusal — a
credential travelling towards a model instead of an approval given through one.

It is a **check**, not the audience. `read_url` also declares
`audience: [AUDIENCE_PERSON]`, and that is worth having, but an audience is a
*listing* rule: the daemon's visibility gate reads verbs, clearance,
compartments and tool sets, and never the audience. A tool relying on its
audience to keep a caller out has no gate at all.

**Which artefacts a person may read is decided per artefact, by the card.**
`read_url` returns a `garm.card.v1.Card`, and the URL, the expiry and the data
key are facts of it, labelled at that artefact's own classification. A card is
the one place in this platform where policy travels on the *value* rather than
on the descriptor, and the daemon knows that one type: at step 8 it walks the
card and clears it for a viewer whose clearance and compartments do not reach
the label. A caller below an artefact's classification gets the id back and
nothing else.

A plain response field could not express this. A field policy is decided at
compile time and says what *every* caller of an RPC may see of a field, while
an artefact store needs the same field readable by one viewer and not the next.
That is the identical reason `garm.tasks.v1` puts a queue's rows in cards.

## No artefact bytes cross NATS

Guaranteed by the shape of the contract and asserted over the service
descriptor, not left to be remembered. No request or response message carries a
`bytes` field, a name that reads like a payload, or an unbounded string; the one
exemption is the 32-byte data key, and it is named with its reason and held to
its bound. NATS has a maximum payload, so a large artefact would fail anyway —
it would fail late, at run time, with a message about a message size rather than
about a design rule.

## The presigned read window, and what it costs both ways

`--read-url-ttl`, default **5 minutes**, logged at startup as
`read_url_ttl=...` so an operator reads the number in force rather than the flag
they did or did not pass.

Short is deliberate: a URL's lifetime is the window in which a leaked link is a
live credential. The other side, which is easy to leave unsaid:

- **While this service is down, no new read can be minted.** Every read of every
  artefact goes through a governed call, so an outage here stops reading, not
  just writing.
- **An outage revokes nothing.** URLs already minted keep working until they
  lapse. There is no revocation list and there cannot be one: the object store
  honours the signature without asking anybody.

So the window is the whole of the exposure and the whole of the recovery time at
once. Five minutes is short enough that a URL copied into a chat log is dead
before anyone reads the log, and long enough for a person to click a link a
client has just rendered. Raise it deliberately for a slow client and know what
you bought.

## Crypto, retention and erasure

**The payload is sealed by a codec, and the daemon is not involved.** The
service generates a data key per artefact, the writer seals with it, and the
service stores only the *wrapped* key. `read_url` has already authorised the
caller and already written the row, so it returns the URL to the ciphertext and
the unwrapped key in the same answer, and the reader decrypts locally. One
governed act releases exactly one artefact's key.

The codec is an interface with a **null implementation**, so a development plane
runs with no key infrastructure at all. That is not what a bank deploys: in
production the wrapping key belongs in a KMS or an HSM and this service holds
only wrapped keys, because a database holding both the ciphertext key and the
wrapped key is a decryption oracle in every backup. The seam is here; the KMS
integration is not. See `KNOWN-GAPS.md`.

**Retention is declared by the creating tool**, in seconds, on `begin_write`.
Where that number comes from is left open — ruling 5 puts the declaration on
`garm.tool.v1.ToolPolicy`, which is a `contracts` release and not this
repository's to make.

**Retention is enforced by shredding, and the row outlives the key.** At
`retain_until` the sweeper destroys the wrapped key and sets `shredded_at`; the
row stays, and so does every disclosure recorded against it. An audit trail must
keep the fact and lose the content. Under the null codec there is no key to
destroy, so the object is deleted as well and the log line says which mechanism
did the work — a deployment reading it learns whether its erasure depends on a
key or on a delete.

The read path therefore has **three** distinct answers, and conflating any two
of them either leaks or misleads:

| | |
|---|---|
| `404` | It never existed, or it is not yours. One answer for both, so the difference cannot be used to enumerate another tenant's ids. |
| `410` | It existed and its key has been destroyed. The record remains; the contents do not. |
| an answer with an id and no card | You may not read this one. The daemon decided that, per artefact, on the way out. |

## Tenancy is a prefix, and the prefix is enforced here

One bucket, `<tenant>/<run>/<artefact_id>`. A bucket per tenant with its own
credential is stronger in principle and was rejected because SeaweedFS's
identity configuration is static — onboarding a tenant would mean editing the
object store — and because the blast radius is bounded anyway: **a presigned URL
is per-object**, so there is no bulk-access capability a mistake could hand out.

A path-construction bug crosses tenants, so the key is built by one function
with a **property test** over it: whatever it is given, either it refuses, or the
key has exactly three components whose first is the tenant it was handed, byte
for byte. There is no third outcome.

Two independent mechanisms keep a caller inside its tenant, and the second holds
even if the first were wrong: the key prefix, and the tenant in the `WHERE`
clause of every read.

## The store

Its own Postgres, under its own role, with two tables — `artefacts` and
`disclosures` — and numbered migrations applied at startup.

Queries are `sqlc`-generated from `internal/store/queries` against the schema the
migrations produce, so a query and a migration that disagree is a build failure
rather than something a person meets. `CLAUDE.md` records why, since this is the
platform's first use of it.

## Concurrency is instances, not goroutines

`tool-go` v0.6.0 runs a handler synchronously in the goroutine its subscription
owns, because `nats.go`'s `micro` reads a request's error field the moment the
handler returns. So `garmtool.WithConcurrency(n)` registers n micro service
instances in one queue group rather than sizing a pool, and every instance is
another responder on `$SRV.INFO`. `--concurrency` defaults to
`garmtool.DefaultConcurrency` — taken from the constant rather than written down
— and raising it is a decision about the whole plane's discovery, not about this
process. Throughput past that is a deployment question: run more processes behind
the queue group.

## Running it

```
artefactd \
  --nats nats://127.0.0.1:4222 \
  --postgres postgres://artefactd:...@127.0.0.1:5432/artefacts \
  --bucket artefacts \
  --s3-endpoint http://127.0.0.1:8333 \
  --clearance INTERNAL
```

Object store credentials come from the AWS SDK's default chain, the way
`garm catalogue publish` takes them. `--bucket` is required: a service that
invented a bucket name would create one and look like it worked.

## Working here

```
mise install    the toolchain
mise run pg     a throwaway Postgres, and the POSTGRES_DSN to export
mise run s3     the environment for the round trip against a real object store
mise run test   go test ./... -race
mise run ci     what CI runs
```

The store and wire tests skip without `POSTGRES_DSN`. CI always sets it.

**The round trip against a real object store is not part of `ci`** and is run
deliberately: `mise run s3`, export what it prints, then `go test -run
TestTheWholeRoundTrip`. It is the test that proves the design works rather than
compiles — begin, the bytes written with nothing but a presigned URL, commit, a
read URL minted through the governed call, and the bytes fetched back by an
ordinary HTTP client. A double cannot tell you that a store honours a signature
this process produced.

## This repository owns its contract

`garm.artefacts.v1` lives here, in `proto/`, generated into `gen/` and committed,
with a drift check over the output. The garm annotations are vendored under
`third_party/proto` and compared byte for byte against the `contracts` version
`go.mod` requires.

`mise run ci` builds a catalogue from the contract and asserts it mounts on a
`garmd`, which is how an annotation that drifted into something this service has
no business declaring is caught here rather than in a deployment.

## The design record is not in this repository

It lives in **[`garm-ai/spec`](https://github.com/garm-ai/spec)** (private),
checked out beside this one at `../spec/docs/superpowers/`. The one that governs
this repository is
`decisions/2026-09-29-artefacts-live-in-s3-behind-a-governed-service.md` — six
rulings, and every section above is one of them.

**Do not create `docs/superpowers/` here.**

## This repository is public

No customer, deployment, tenant or internal hostname appears anywhere in it.
Fixtures use `example.com` shapes and generic subjects.

MIT licensed.
