import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const chat = readFileSync(new URL("../src/chat.ts", import.meta.url), "utf8");
const css = readFileSync(new URL("../src/shell.css", import.meta.url), "utf8");

test("finished turns keep only tools and approvals inside one consolidated activity fold", () => {
  assert.match(chat, /const foldItems: TimelineItem\[\] = \[\]/);
  assert.match(chat, /it\.kind === "tool" \|\| it\.kind === "approval"/);
  assert.match(chat, /class="work-said"/);
  assert.match(chat, /<details class="work-fold"/);
  assert.equal((chat.match(/<details class="work-fold"/g) ?? []).length, 1);
});

test("text before a later tool is hidden so evidence-backed final text is not duplicated", () => {
  assert.match(chat, /const lastToolIndex = timeline\.findLastIndex/);
  assert.match(chat, /it\.kind === "text" && !demoted && index > lastToolIndex/);
});

test("reasoning content is not rendered as chain-of-thought", () => {
  assert.ok(!/markdown\(chunk\.thinking\)/.test(chat));
  assert.ok(!/return thinkingRow\(item\.activity\)/.test(chat));
});

test("live work shows recent tool steps instead of only a generic thinking label", () => {
  assert.match(chat, /function liveWorkSteps\(work: WorkBlock\): TimelineItem\[\]/);
  assert.match(chat, /recentSteps\.length > 1/);
  assert.match(chat, /class="live-work-steps"/);
  assert.match(css, /\.live-work-steps \{/);
});

test("streaming turns expose execution telemetry instead of only Thinking", () => {
  assert.match(chat, /function streamingStatusRow\(\)/);
  assert.match(chat, /isStreaming \? streamingStatusRow\(\) : typingRow\(\)/);
  assert.match(chat, /workPhaseTrail\(work\)/);
  assert.match(css, /\.execution-progress \{/);
});

test("completed turns retain a safe phase trail without rendering chain-of-thought", () => {
  assert.match(chat, /function workPhaseTrail\(work: WorkBlock\)/);
  assert.match(chat, /class="work-trace"/);
  assert.match(css, /\.work-trace-step \{/);
  assert.ok(!/activity\.payload\.thinking/.test(chat));
});

test("the fold summary counts tool calls only — spoken text is never tallied as a hidden 'message'", () => {
  assert.match(chat, /function segmentSummaryLabel\(/);
  assert.ok(!/message\$\{messages === 1/.test(chat), "old 'N messages' summary should be gone");
  assert.match(chat, /it\.kind === "tool"/);
});

test("promoted speech keeps full reply styling", () => {
  assert.match(css, /\.work-said \{[\s\S]{0,200}?color: var\(--foreground\);/);
});

test("a demoted post-delivery self-log stays hidden — never promoted to speech", () => {
  const bridge = readFileSync(new URL("../src/core-bridge.ts", import.meta.url), "utf8");
  assert.match(bridge, /payload: \{ text, demoted: true \}/);
  assert.match(chat, /demoted === true/);
  assert.match(chat, /it\.kind === "text" && !demoted/);
});

test("the fold chevron rotates when a work-fold is open", () => {
  assert.match(css, /\.work-fold\[open\] > summary\.work-head \.icon \{[\s\S]{0,80}?transform: rotate\(90deg\);/);
});
