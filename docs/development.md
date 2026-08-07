# Development and Validation

## Repository Direction

- Primary language: Go.
- PostgreSQL is derived state; Markdown/raw artifacts remain authoritative.
- Keep HTTP handlers thin and reuse compiler/service/wiki functions.
- Keep client integrations as API clients; do not move knowledge semantics into
  a browser. The standalone Knowledge Core Web UI has been removed; QM owns the
  user-facing Web UI.
- Use versioned wiki writes and safe project-relative generated paths.
- Prefer standard library and conservative dependencies.

Read [AGENTS.md](../AGENTS.md) before implementation work. It contains the full
LLM Wiki, GitNexus, graphify, sync, query, and validation constraints.

## Go Checks

Use a writable cache in restricted environments:

```bash
env GOCACHE=/private/tmp/kbcore-gocache go test ./...
```

Tests should prove behavior through real temporary project directories where
practical. Production embedding behavior must use an injected provider; fake
vectors belong only in tests.

## Deterministic Agent Validation

`tst/xiyouji.txt` is split into 100 chapter files under
`tst/xiyouji-chapters/`. Run the package tests:

```bash
env GOCACHE=/private/tmp/knowlega-gocache \
  go test ./internal/agent/knowlega/compiler ./internal/agent/knowlega/service ./internal/agent/knowlega
```

The deterministic scaffold validates the accumulation shape:

- 100 immutable sources;
- 300 generated wiki pages;
- 100 review items in the mock fixture;
- no broken wikilinks;
- navigation-first query actions and evidence auto-read;
- graphify evidence use for a code relationship question.

Mock/offline agents validate plumbing only. They must not be described as real
semantic LLM reasoning and cannot write syntheses.

## Real-LLM Acceptance

Copy `configs/qm-config.example.yaml` to the ignored
`qm-backend/configs/config.yaml`, add credentials, and run the QM backend; then
exercise file/memory/conversation writes through the QM HTTP API. The resulting
raw artifacts and `.kbcore/ingest-queue.json` are the durable handoff to the
Agent; maintenance consumes the queue and runs semantic review when configured.

The package tests cover the deterministic compiler/query loop. A credentialed
deployment must additionally verify source summaries, provenance, wikilinks,
`wiki/overview.md`, `wiki/reviews.md`, structural lint, semantic review, and
version archives.

Use the real provider for product acceptance. Use deterministic agents only
when a test explicitly targets orchestration, parsing, validation, or storage
plumbing. Keep the QM backend as the only runnable product entrypoint.
