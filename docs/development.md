# Development and Validation

## Repository Direction

- Primary language: Go.
- PostgreSQL is derived state; Markdown/raw artifacts remain authoritative.
- Keep HTTP handlers thin and reuse compiler/service/wiki functions.
- Keep the Web UI as an API client; do not move knowledge semantics into the
  browser.
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

## Web Checks

```bash
cd web
npm install
npm test -- --run
npm run build
```

The current build may report Vite chunk-size warnings; treat test or type/build
failures as blocking, and evaluate chunk splitting separately as a frontend
performance task.

## Deterministic Accumulation Validation

`tst/xiyouji.txt` is split into 100 chapter files under
`tst/xiyouji-chapters/`. Run:

```bash
bash scripts/verify-xiyouji.sh
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

Run the small credentialed acceptance with:

```bash
bash scripts/verify-llmwiki-llm.sh config.yaml
```

The script creates three fixed sources and verifies:

- analysis/generation and one source-summary page per source;
- source provenance and wikilinks;
- `wiki/overview.md` and `wiki/reviews.md` maintenance;
- deterministic `lint` returning `ok`;
- semantic findings from both `review-wiki --agent llm` and
  `lint --agent llm` for the deliberately stale fixture claim.

On success it writes `VERIFY_REPORT.md` inside the temporary wiki project with
source, generated-page, link, lint, and review evidence. Missing YAML or real
credentials is a hard failure; the script must not silently fall back to mock.

For the real 100-chapter constraint-query acceptance, reuse a completed
Xiyouji wiki and run:

```bash
bash scripts/verify-xiyouji-query-llm.sh config.yaml /private/tmp/kbcore-xiyouji-wiki
```

This check requires 唐太宗 as the canonical candidate, supported evidence for
requirements 1–8, and a corpus-scoped `not_found_in_corpus` result for the
ninth negative requirement.

## Manual Corpus Flow

```bash
env GOCACHE=/private/tmp/kbcore-gocache \
  go run ./cmd/kbcore --config config.yaml init \
  --path /private/tmp/kbcore-xiyouji-wiki --name xiyouji

env GOCACHE=/private/tmp/kbcore-gocache \
  go run ./cmd/kbcore --config config.yaml validate-llmwiki \
  --project /private/tmp/kbcore-xiyouji-wiki \
  --source tst/xiyouji-chapters --agent llm --skip-unchanged

env GOCACHE=/private/tmp/kbcore-gocache \
  go run ./cmd/kbcore --config config.yaml lint \
  --project /private/tmp/kbcore-xiyouji-wiki
```

Use the real provider for product acceptance. Use deterministic agents only
when a test explicitly targets orchestration, parsing, validation, or storage
plumbing.
