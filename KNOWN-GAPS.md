# Known gaps

What is not built, what is half built, and what was deliberately left to another
release or another repository. Gaps only — what IS built is in README.md. Kept
honest by the changes that close the entries.

## Where the decision did not survive contact with the code

**Ruling 4 says the daemon checks the caller against the artefact's
classification. It can, but only through a card, and nothing can tell this
service what an individual artefact should be classified at.**

Two halves, and the first one is fine. `garm.tool.v1.InvocationContext`
deliberately carries no clearance and no compartments — the tool-service-shell
spec says in three places that there is no legitimate tool-side use — so a
comparison in this service is impossible by design. What exists instead is the
daemon's card walk: `garm.card.v1.Card` carries its policy on the *value*, and
step 8 drops what the viewer does not reach. `read_url` therefore releases
through a card, and ruling 4 holds exactly as written, with the daemon doing the
comparing. That is a finding rather than a gap: the ruling is implementable, and
only through a card.

The gap is the second half. **The classification is one value for the whole
deployment**, configured with `--clearance` and `--compartments`, because nothing
in the platform can yet say that *this* artefact is CONFIDENTIAL and that one is
INTERNAL. The creating tool is the only thing that knows, and it would declare it
the way ruling 5 has it declare retention — a field on
`garm.tool.v1.ToolPolicy`, which is a `contracts` release and not this
repository's to make. Until then every artefact a deployment holds carries the
same label, and a deployment that needs two sets them up as two deployments or
raises the floor to the higher of the two.

**A `read_url` refusal by the daemon and a successful mint are indistinguishable
in this service's own record.** The disclosure row is written before the card is
built, so it records that a URL was minted for that caller at that label; whether
the daemon then withheld the card is the daemon's own ledger row for the same
`call_id`. The two are joinable and the pair is truthful, but neither row alone
says "this person actually received a credential". Closing it properly needs the
daemon to report a withheld field back to the tool, which nothing does.

## Not built

**A KMS or an HSM, and therefore production-grade sealing.** Ruling 6 is clear
that the wrapping key belongs in a KMS and that a service database holding both
the ciphertext key and the wrapped key is a decryption oracle in every backup.
What is here is the seam: `internal/codec.Codec` mints and wraps a data key per
artefact, `Name()` records the algorithm per artefact so a future codec can open
an old object, and `KeyID()` records the wrapping key and its version so a
rotation is expressible. The only implementation is `codec.Null`, which seals
nothing, and it is what the development plane runs. **A deployment running the
null codec is protected by the object store's access control and by nothing
else**, and the sweeper's log line says so: with no key to destroy, erasure is a
delete.

No codec that actually encrypts is written, and no nonce or IV is stored — a
codec that needs one writes it into the payload it seals, and would say so in its
own documentation. The bounded 32-byte `data_key` field on
`BeginWriteResponse` is the seam's wire shape and is empty under the null codec.

**Where the retention value comes from.** Ruling 5 puts the declaration on
`garm.tool.v1.ToolPolicy`, beside the verb and the clearance, so a scratch
preview declares hours and a document about a payment declares years and the
number is reviewed before it ships. That is a `contracts` release, and the lint
rule deciding whether it is required on a WRITE tool that produces an artefact
comes with it. So `begin_write` takes `retention_seconds` as a request field and
the artefact carries it; **where a generator gets the number is open**, and today
it is whatever that generator passes.

**A `shred` method.** Crypto-shredding is expressible in the record and the
sweeper performs it at `retain_until`, which is how retention is enforced. There
is no governed call to shred an artefact early, which is what a subject-erasure
request would need. It would be a `VERB_DESTRUCTIVE` tool with an approval gate,
and that is a decision record rather than a field.

**A `list`.** Nothing browses. A generator hands its caller an id, and a person's
client shows what the run put in front of them. A listing needs a per-artefact
visibility answer for a *page* of artefacts, which the card mechanism supports —
`garm.tasks.v1.list_tasks` returns a page of cards and the daemon drops the ones
the viewer does not reach — so the shape is known and the method is simply not
written.

**Nothing about `garmd`.** The decision rejects putting decryption there on four
counts: the bytes would cross the daemon, it would give the daemon key material
where today it holds none, it contradicts the rule that the daemon holds no state
outliving a request, and it collapses the presigned read path. Nothing in this
repository touches that repository, and `mise run no-enforcement` asserts there
is not even a path in the dependency graph.

**Not in any compose file.** This service is not added to the development plane.
It has been run against the plane's SeaweedFS from a test, in its own bucket,
which is what the round trip covers.

## Where a check is thinner than the design

**`commit` verifies a size and not a digest.** The object store is asked what it
holds at the key and a size that disagrees is refused, which is as far as this
service can check without reading a payload it must never read. The plaintext
digest is the writer's claim, recorded for the reader to verify — which is the
right place for it, since the reader is the one holding the bytes, but it does
mean a writer that lies about its digest is not caught here.

**The presigned PUT is not bounded by size.** `begin_write` signs a write for one
object at one key and the signature says nothing about how large it may be. S3
supports a signed content-length only through POST policy documents, which is a
different signing scheme. So a generator can write an object larger than it later
claims, and `commit` refuses the mismatch — the artefact is never readable, but
the bytes were stored and the sweeper removes them at the upload window.

**`tool_fqn` is the caller's assertion.** The invocation names the principal, the
tenant and the run; it does not name which tool is calling. So the creating tool's
FQN on the row is provenance supplied by the generator, not verified. A generator
that named another tool would be lying about its own work, which is a different
class of problem from a caller escalating its own reach.

**`describe` is statically `CLEARANCE_PUBLIC` at the field level.** Its response
is metadata — type, size, label, classification, state — and the method gate is
PUBLIC like every other method here, so any caller in the tenant holding the
`artefacts` tool set learns that an artefact exists and what it is, whatever it is
classified at. Only the *contents* are per-artefact. Making the metadata
per-artefact too would mean answering `describe` through a card as well, which is
a bigger change than this release needed and is worth doing when somebody wants
it.

**An orphan's object is deleted before its row.** If the delete succeeds and the
row delete then fails, the next sweep tries again and the second delete of a
missing object succeeds, so the pass is idempotent. The other order would leave an
object nothing points at, which nothing would ever clean up.

**The sweeper runs on every instance.** Both passes are idempotent and bounded to
200 rows, so several instances do the same work rather than the wrong work. A
leader election is not worth it at this size.

## Left to another release

**No metrics and no tracing.** The broker's own service statistics are what there
is. The trace context is on the invocation and is not propagated.

**No ledger rows of this service's own, and that is the platform's rule rather
than an omission.** The per-call ledger row is `garmd`'s: a tool service writing
to `garm.v1.ledger.>` is a second producer for one call, which is how a row gets
counted twice, and the broker perimeter design puts that subject in the denied
column for a `garmtool` service. What this service keeps instead is the
`disclosures` table, which is its own record and is what the audit trail of a
disclosure is actually made of.

**The generated cards are the generated defaults.** `protoc-gen-garm-go`
synthesises an input card and a result card beside every tool, and the result
cards answer `result_unavailable` because a card about an answer needs the answer
and this service keeps no per-call response store. `read_url`'s own release card
is built by hand in `internal/artefacts/card.go` and is the one that matters.

**No conformance suite.** `tool-go`'s `conformance/` does not exist yet, so
nothing here runs the suite `garm` publishes against this service.
