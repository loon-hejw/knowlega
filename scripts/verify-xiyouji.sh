#!/usr/bin/env bash
set -euo pipefail

CONFIG="${1:-config.yaml}"
ROOT="$(mktemp -d /private/tmp/kbcore-xiyouji-verify.XXXXXX)"
CACHE="${GOCACHE:-/private/tmp/kbcore-gocache}"

run() {
  env GOCACHE="$CACHE" go run ./cmd/kbcore --config "$CONFIG" "$@"
}

expect_line() {
  local file="$1"
  local expected="$2"
  if ! grep -qx "$expected" "$file"; then
    echo "expected line not found: $expected" >&2
    echo "--- $file ---" >&2
    cat "$file" >&2
    exit 1
  fi
}

first_evidence_line() {
  awk '
    /^results:$/ { in_results = 1; next }
    in_results && /^1\. / { print; exit }
    /^trace:$/ { in_trace = 1; next }
    in_trace && /action=read/ { print; exit }
  ' "$1"
}

assert_chapter_result() {
  local file="$1"
  local query="$2"
  local line
  line="$(first_evidence_line "$file")"
  if [[ -z "$line" ]]; then
    echo "no first result or read evidence for query: $query" >&2
    cat "$file" >&2
    exit 1
  fi
  if [[ "$line" == *"wiki/index.md"* || "$line" == *"wiki/overview.md"* || "$line" == *"wiki/log.md"* ]]; then
    echo "aggregate page dominated query '$query': $line" >&2
    exit 1
  fi
  if [[ "$line" != *"chapter-"* ]]; then
    echo "expected chapter page for query '$query', got: $line" >&2
    exit 1
  fi
}

mkdir -p "$ROOT"
echo "project=$ROOT"

run init --path "$ROOT" --name xiyouji >/tmp/kbcore-xiyouji-init.out

VALIDATE_OUT=/tmp/kbcore-xiyouji-validate.out
run validate-llmwiki \
  --project "$ROOT" \
  --source tst/xiyouji-chapters \
  --agent mock >"$VALIDATE_OUT"

expect_line "$VALIDATE_OUT" "sources=100"
expect_line "$VALIDATE_OUT" "files=300"
expect_line "$VALIDATE_OUT" "reviews=100"
expect_line "$VALIDATE_OUT" "skipped=0"

LINT_OUT=/tmp/kbcore-xiyouji-lint.out
run lint --project "$ROOT" >"$LINT_OUT"
expect_line "$LINT_OUT" "ok"

GRAPH_JSON=/tmp/kbcore-xiyouji-graphify.json
cat >"$GRAPH_JSON" <<'JSON'
{
  "nodes": [
    {"id":"auth.validate","kind":"function","label":"ValidateToken","source_file":"internal/auth/token.go"},
    {"id":"auth.service","kind":"class","label":"AuthService","source_file":"internal/auth/service.go"}
  ],
  "edges": [
    {"source":"auth.validate","target":"auth.service","relation":"calls","confidence":"EXTRACTED","weight":1}
  ]
}
JSON

CODE_IMPORT_OUT=/tmp/kbcore-xiyouji-code-import.out
run code-import-graphify \
  --project "$ROOT" \
  --repo-id demo-repo \
  --repo-path /tmp/demo-repo \
  --graph "$GRAPH_JSON" >"$CODE_IMPORT_OUT"
expect_line "$CODE_IMPORT_OUT" "snapshot=raw/code-graphs/demo-repo/graphify"
expect_line "$CODE_IMPORT_OUT" "overview=wiki/code/demo-repo/overview.md"
expect_line "$CODE_IMPORT_OUT" "nodes=2"
expect_line "$CODE_IMPORT_OUT" "edges=1"

HGS_OUT=/tmp/kbcore-xiyouji-query-huaguoshan.out
run query --project "$ROOT" --q 花果山 --limit 5 --agent mock >"$HGS_OUT"
expect_line "$HGS_OUT" "intent=answer_from_persistent_wiki"
expect_line "$HGS_OUT" "mode=mock_tool_loop"
expect_line "$HGS_OUT" "writeback=false"
grep -q 'action=list_pages' "$HGS_OUT"
grep -q 'action=final' "$HGS_OUT"
assert_chapter_result "$HGS_OUT" "花果山"

HSH_OUT=/tmp/kbcore-xiyouji-query-heishuihe.out
run query --project "$ROOT" --q 黑水河 --limit 5 --agent mock >"$HSH_OUT"
expect_line "$HSH_OUT" "intent=answer_from_persistent_wiki"
expect_line "$HSH_OUT" "mode=mock_tool_loop"
expect_line "$HSH_OUT" "writeback=false"
grep -q 'action=list_pages' "$HSH_OUT"
grep -q 'action=search' "$HSH_OUT"
grep -q 'auto-read' "$HSH_OUT"
grep -q 'action=final' "$HSH_OUT"
assert_chapter_result "$HSH_OUT" "黑水河"

GRAPH_QUERY_OUT=/tmp/kbcore-xiyouji-query-graph.out
run query --project "$ROOT" --q "ValidateToken AuthService calls" --limit 5 --agent mock >"$GRAPH_QUERY_OUT"
expect_line "$GRAPH_QUERY_OUT" "intent=answer_from_persistent_wiki"
expect_line "$GRAPH_QUERY_OUT" "mode=mock_tool_loop"
expect_line "$GRAPH_QUERY_OUT" "writeback=false"
grep -q 'action=graph' "$GRAPH_QUERY_OUT"
grep -q 'raw/code-graphs/demo-repo/graphify/graph.json' "$GRAPH_QUERY_OUT"
grep -q 'action=final' "$GRAPH_QUERY_OUT"

echo "ok: xiyouji verification passed"
echo "wiki=$ROOT"
