#!/usr/bin/env bash
set -euo pipefail

CONFIG="${1:-config.yaml}"
PROJECT="${2:-/private/tmp/kbcore-xiyouji-wiki}"
CACHE="${GOCACHE:-/private/tmp/kbcore-gocache}"
OUT="${KBQUERY_VERIFY_OUT:-/private/tmp/kbcore-xiyouji-query-llm.out}"

if [[ ! -f "$CONFIG" ]]; then
  echo "error: YAML config with real LLM credentials not found: $CONFIG" >&2
  exit 1
fi
if [[ ! -f "$PROJECT/wiki/entities/唐太宗.md" || ! -f "$PROJECT/.kbcore/source-manifest.json" ]]; then
  echo "error: expected an initialized real 100-chapter wiki project: $PROJECT" >&2
  exit 1
fi

QUESTION=$'1.有结义的情节\n\n2.见过孙悟空\n\n3.跟孙悟空不算敌对关系\n\n4.出场不止一次\n\n5.曾逼迫唐僧做了某事\n\n6.最后唐僧就范,而且没受到什么损失\n\n7.见过阎罗王\n\n8.见过观音\n\n9.不曾到过花果山'

env GOCACHE="$CACHE" go run ./cmd/kbcore --config "$CONFIG" query \
  --project "$PROJECT" \
  --q "$QUESTION" \
  --limit 12 \
  --agent llm \
  --db-dsn "" >"$OUT"

require_line() {
  local expected="$1"
  if ! grep -qx "$expected" "$OUT"; then
    echo "expected line not found: $expected" >&2
    echo "--- $OUT ---" >&2
    sed -n '1,260p' "$OUT" >&2
    exit 1
  fi
}

require_pattern() {
  local pattern="$1"
  if ! grep -Eq "$pattern" "$OUT"; then
    echo "expected pattern not found: $pattern" >&2
    echo "--- $OUT ---" >&2
    sed -n '1,260p' "$OUT" >&2
    exit 1
  fi
}

require_line "status=complete"
require_line "candidate=唐太宗"
for id in 1 2 3 4 5 6 7 8; do
  require_pattern "^- requirement=${id} status=supported "
done
require_pattern '^- requirement=9 status=not_found_in_corpus '
require_pattern '^答案.*唐太宗|唐太宗'
require_pattern 'chapter-012|wiki/entities/唐太宗.md'

if grep -Eq '^incomplete_reason=|未完成核验：1\.' "$OUT"; then
  echo "query incorrectly returned an incomplete or all-requirements fallback" >&2
  sed -n '1,260p' "$OUT" >&2
  exit 1
fi
if awk '/^citations:$/,/^results:$/' "$OUT" | grep -Eq 'wiki/(index|overview|log|reviews)\.md'; then
  echo "aggregate navigation page leaked into final citations" >&2
  sed -n '1,260p' "$OUT" >&2
  exit 1
fi

echo "ok: real-LLM xiyouji constraint query passed"
echo "report=$OUT"
