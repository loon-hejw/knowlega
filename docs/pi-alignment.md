# Go Pi alignment and knowledge recovery

The user-facing reasoning agent remains the Go Pi adapter. Knowledge Core is a
deterministic tool, not another reasoning agent. `knowledge submit` is an optional
evidence-ledger check: complete and incomplete results both return to the model.
The model owns the final response. Explicit Wiki writeback still requires a
complete validated submission and user authorization.

## Reference and repeatable checks

The reference is TS Pi **v0.85.1**, commit
`d981de1229ef899957bbe968bc8dcda02a21f477`, specifically
[`packages/agent/src/agent-loop.ts`](https://github.com/earendil-works/pi/blob/v0.85.1/packages/agent/src/agent-loop.ts).
This is a development reference, not a new Node backend or a browser dependency
upgrade.

From the repository root, regenerate the fixtures with Node 24:

```sh
npm --prefix scripts/pi-reference install --ignore-scripts
npm --prefix scripts/pi-reference run generate
env GOCACHE=/private/tmp/knowlega-gocache go test ./internal/qm/agent
```

The generator executes the pinned upstream runtime using scripted model output.
The Go differential test compares model call counts, executed call IDs, and final
response text for six shared cases: complete submit, incomplete submit, recoverable
tool error, repeated calls, truncated output, and an empty natural completion.
The stored TS event trace is diagnostic; the Go test does not claim full event
protocol equivalence.

Separate Go tests cover explicit parallel tool execution, steering versus
follow-up queues, argument validation, and context compaction preserving complete
tool batches. Core tool schemas are checked for their supported vocabulary:
objects, arrays, primitive types, required properties, enums, additional-property
restrictions, numeric bounds, and string patterns. Tool handlers retain semantic
validation and authorization.

## Deliberate host behavior

- QM retains permissions, durable session/tape records, cancellation and finite
  execution budgets. Knowledge validation no longer adds its own loop budgets,
  repeated-call suppression, forced submission, or final-answer replacement.
- Only tools explicitly marked parallel may execute concurrently. A sequential
  member or an approval gate makes the batch sequential. Results are persisted in
  model order after the batch finishes; TS incremental event timing is not copied.
- Context pressure uses a summary from the same configured model and preserves
  whole recent tool batches. This is a host context transformation, not a hidden
  knowledge verifier. Token estimation remains approximate; compaction failure is
  explicit and the original tape is retained.
- Truncated model output never dispatches its tool calls. Invalid arguments and
  unknown tools become recoverable tool errors rather than executable empty input.

## Knowledge and corpus changes

Candidate names may be searched without an existing entity page. Candidate aliases
boost matching results without discarding event evidence that omits the name.
`scope=all|wiki|raw` selects search roots before ranking and limiting results.
Search is navigation; read, follow_links and graph establish evidence.

Convergence preserves valid frontmatter provenance even when manifest ownership
is incomplete. Migration version 6 restores missing quarantined pages from their
latest archived version with active raw-source provenance, then converges the
manifest. Raw sources and archives are retained. Generation rejects observed
placeholder/planning artifacts and asks the compiler to preserve event actors,
refusal, coercion and outcomes in the source language.

Local recovery restored 791 pages, yielding 1,093 active pages. PostgreSQL was
resynchronized with 100 sources, 100 manifest entries, 4,622 page versions and 216
review entries. A pre-repair archive remains under ignored `config.local/`.

The remaining live acceptance is intentionally distinct from these code checks:
versioned regeneration of 14 polluted chapter summaries plus chapter 12, then
three fresh runs of the original nine-condition question using the existing YAML
model configuration. Inspect the actual raw evidence, candidate identity, handling
of each condition and the scope of negative claims; do not force a complete submit
or inject an expected answer into the prompt. These external-model operations
require the pending explicit authorization and have not yet been run.
