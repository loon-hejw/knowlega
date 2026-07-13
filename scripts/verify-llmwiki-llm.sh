#!/usr/bin/env bash
set -euo pipefail

CONFIG="${1:-config.yaml}"
if [[ ! -f "$CONFIG" ]]; then
  echo "error: YAML config not found: $CONFIG" >&2
  exit 1
fi
ROOT="$(mktemp -d /private/tmp/kbcore-llmwiki-verify.XXXXXX)"
SOURCES="$ROOT/input-sources"
CACHE="${GOCACHE:-/private/tmp/kbcore-gocache}"

run() {
  env GOCACHE="$CACHE" go run ./cmd/kbcore --config "$CONFIG" "$@"
}

require_grep() {
  local pattern="$1"
  local file="$2"
  if ! grep -Eq "$pattern" "$file"; then
    echo "expected pattern not found: $pattern" >&2
    echo "--- $file ---" >&2
    cat "$file" >&2
    exit 1
  fi
}

SEMANTIC_ISSUE_PATTERN='(^|[[:space:]])(contradiction|duplicate|missing-page|stale-claim|source-gap|review-needed)([[:space:]]|$)'

require_semantic_issue() {
  local file="$1"
  local label="$2"
  if ! grep -Eq "$SEMANTIC_ISSUE_PATTERN" "$file"; then
    echo "expected $label to report a semantic wiki issue" >&2
    echo "--- $file ---" >&2
    cat "$file" >&2
    exit 1
  fi
}

is_aggregate_page() {
  case "$1" in
    wiki/index.md|wiki/overview.md|wiki/reviews.md|wiki/log.md) return 0 ;;
    *) return 1 ;;
  esac
}

mkdir -p "$SOURCES"
echo "project=$ROOT"

cat >"$SOURCES/garden.md" <<'MD'
# North Garden Rose Bed

The North Garden rose bed uses compost blend A during spring planting. The bed
label points readers to the spring garden notes.
MD

cat >"$SOURCES/watering.md" <<'MD'
# Rose Bed Watering Notes

The watering notes also name compost blend A for the North Garden rose bed. The
watering calendar links the rose bed to spring garden notes.
MD

cat >"$SOURCES/old-note.md" <<'MD'
# Old Garden Note

An older garden note says the North Garden rose bed uses compost blend B only.
This note looks stale because the current garden and watering notes both name
compost blend A.
MD

run init --path "$ROOT" --name llmwiki-acceptance >/tmp/kbcore-llmwiki-init.out

VALIDATE_OUT=/tmp/kbcore-llmwiki-validate.out
run validate-llmwiki --project "$ROOT" --source "$SOURCES" --agent llm >"$VALIDATE_OUT"

require_grep '^sources=3$' "$VALIDATE_OUT"
require_grep '^file=wiki/' "$VALIDATE_OUT"
require_grep '^reviews=[1-9][0-9]*$' "$VALIDATE_OUT"

mapfile -t GENERATED_FILES < <(awk -F= '/^file=wiki\// { print $2 }' "$VALIDATE_OUT" | sort -u)
if (( ${#GENERATED_FILES[@]} < 3 )); then
  echo "expected at least three generated wiki files, got ${#GENERATED_FILES[@]}" >&2
  cat "$VALIDATE_OUT" >&2
  exit 1
fi

CONTENT_FILES=()
for rel in "${GENERATED_FILES[@]}"; do
  path="$ROOT/$rel"
  if [[ ! -s "$path" ]]; then
    echo "expected generated page to exist: $rel" >&2
    exit 1
  fi
  if is_aggregate_page "$rel"; then
    continue
  fi
  CONTENT_FILES+=("$rel")
  if [[ "$(head -n 1 "$path")" != "---" ]]; then
    echo "generated page missing YAML frontmatter: $rel" >&2
    cat "$path" >&2
    exit 1
  fi
  require_grep '^type:' "$path"
  require_grep '^title:' "$path"
  require_grep '^sources:' "$path"
  require_grep 'raw/sources/' "$path"
  require_grep '^# ' "$path"
  if ! grep -q -- "\`$rel\`" "$ROOT/wiki/index.md"; then
    echo "wiki/index.md missing generated page: $rel" >&2
    cat "$ROOT/wiki/index.md" >&2
    exit 1
  fi
done

if (( ${#CONTENT_FILES[@]} < 3 )); then
  echo "expected at least three non-aggregate generated wiki content files, got ${#CONTENT_FILES[@]}" >&2
  cat "$VALIDATE_OUT" >&2
  exit 1
fi

source_summary_count=0
for rel in "${CONTENT_FILES[@]}"; do
  if [[ "$rel" == wiki/sources/* ]]; then
    source_summary_count=$((source_summary_count + 1))
  fi
done
if (( source_summary_count < 3 )); then
  echo "expected one source-summary page per fixed source, got $source_summary_count" >&2
  cat "$VALIDATE_OUT" >&2
  exit 1
fi

for rel in wiki/index.md wiki/overview.md wiki/reviews.md; do
  if [[ ! -s "$ROOT/$rel" ]]; then
    echo "expected non-empty $rel" >&2
    exit 1
  fi
done

link_matches="$(grep -Roh '\[\[[^]]\+\]\]' "$ROOT/wiki" || true)"
link_count="$(printf '%s\n' "$link_matches" | sed '/^$/d' | wc -l | tr -d ' ')"
if (( link_count < 2 )); then
  echo "expected generated wiki to contain multiple wikilinks, got $link_count" >&2
  exit 1
fi

require_grep 'Recent LLM Wiki Updates|North Garden|Rose|Compost|watering' "$ROOT/wiki/overview.md"
require_grep '^## \[' "$ROOT/wiki/reviews.md"
require_grep 'raw/sources/' "$ROOT/wiki/reviews.md"

LINT_OUT=/tmp/kbcore-llmwiki-lint.out
run lint --project "$ROOT" >"$LINT_OUT"
if ! grep -qx 'ok' "$LINT_OUT"; then
  echo "deterministic lint did not pass cleanly" >&2
  cat "$LINT_OUT" >&2
  exit 1
fi

REVIEW_OUT=/tmp/kbcore-llmwiki-review.out
run review-wiki --project "$ROOT" --agent llm >"$REVIEW_OUT"
if [[ ! -s "$REVIEW_OUT" ]]; then
  echo "expected review-wiki to produce output" >&2
  exit 1
fi
require_semantic_issue "$REVIEW_OUT" "review-wiki --agent llm"

LINT_LLM_OUT=/tmp/kbcore-llmwiki-lint-llm.out
run lint --project "$ROOT" --agent llm >"$LINT_LLM_OUT"
if [[ ! -s "$LINT_LLM_OUT" ]]; then
  echo "expected lint --agent llm to produce output" >&2
  exit 1
fi
require_semantic_issue "$LINT_LLM_OUT" "lint --agent llm"

REPORT="$ROOT/VERIFY_REPORT.md"
{
  echo "# LLM Wiki Verification Report"
  echo
  echo "- Project: \`$ROOT\`"
  echo "- Sources: 3"
  echo "- Generated files: ${#GENERATED_FILES[@]}"
  echo "- Generated content files: ${#CONTENT_FILES[@]}"
  echo "- Source-summary files: $source_summary_count"
  echo "- Wikilinks found: $link_count"
  echo "- Semantic review issue check: passed"
  echo
  echo "## Fixed Sources"
  echo
  for source in "$SOURCES"/*.md; do
    echo "- \`$source\`"
  done
  echo
  echo "## Generated Wiki Files"
  echo
  for rel in "${GENERATED_FILES[@]}"; do
    echo "- \`$rel\`"
  done
  echo
  echo "## Generated Content Files"
  echo
  for rel in "${CONTENT_FILES[@]}"; do
    echo "- \`$rel\`"
  done
  echo
  echo "## validate-llmwiki --agent llm"
  echo
  echo '```text'
  cat "$VALIDATE_OUT"
  echo '```'
  echo
  echo "## lint"
  echo
  echo '```text'
  cat "$LINT_OUT"
  echo '```'
  echo
  echo "## review-wiki --agent llm"
  echo
  echo '```text'
  cat "$REVIEW_OUT"
  echo '```'
  echo
  echo "## lint --agent llm"
  echo
  echo '```text'
  cat "$LINT_LLM_OUT"
  echo '```'
} >"$REPORT"

echo "ok: llmwiki LLM verification passed"
echo "wiki=$ROOT"
echo "report=$REPORT"
