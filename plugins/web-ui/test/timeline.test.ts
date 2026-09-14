import { test } from "node:test";
import assert from "node:assert/strict";
import type { ToolActivity, WorkBlock } from "../src/core-bridge.ts";
import { buildTimeline, toolFailureLabel, toolRowKind, type ToolRowModel } from "../src/timeline.ts";

function act(seq: number, type: ToolActivity["type"], payload: unknown): ToolActivity {
  return { seq, parentSeq: null, type, payload, createdAt: seq };
}

function row(call: unknown, result?: unknown): ToolRowModel {
  return { call: act(1, "tool_call", call), result: result === undefined ? null : act(2, "tool_result", result) };
}

test("thinking, tool calls, and results stay in seq order — never bucketed by type", () => {
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(1, "thinking", { thinking: "first I'll look" }),
      act(2, "tool_call", { tool: "execute", command: "ls" }),
      act(3, "tool_result", { tool: "execute", code: 0 }),
      act(4, "thinking", { thinking: "now read it" }),
      act(5, "tool_call", { tool: "read", path: "/x" }),
      act(6, "tool_result", { tool: "read" }),
    ],
  };
  const items = buildTimeline(work);
  assert.deepEqual(
    items.map((i) => i.kind),
    ["thinking", "tool", "thinking", "tool"],
    "two thinking blocks interleave between the two tool rows, in arrival order",
  );
  assert.equal(
    items[0].kind === "thinking" && (items[0].activity.payload as { thinking: string }).thinking,
    "first I'll look",
  );
  assert.ok(
    items[1].kind === "tool" && items[1].row.call && items[1].row.result,
    "the first tool row pairs its call with its result",
  );
});

test("a paused command's approval marker attaches to its blocked tool row", () => {
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(1, "tool_call", { tool: "execute", command: "rm -rf x" }),
      act(2, "tool_result", { tool: "execute", blocked: "needs_approval", reason: "recursive delete" }),
    ],
    pendingApprovals: [{ requestId: "r1", command: "rm -rf x", reason: "recursive delete" }],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 1, "no separate approval item — the marker rides the blocked tool row");
  assert.ok(items[0].kind === "tool" && items[0].row.pending?.requestId === "r1");
});

test("a command approval's plain-English summary survives onto the timeline item for the renderer", () => {
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(1, "tool_call", { tool: "execute", command: "rm -rf x" }),
      act(2, "tool_result", { tool: "execute", blocked: "needs_approval", reason: "recursive delete" }),
    ],
    pendingApprovals: [
      {
        requestId: "r1",
        command: "rm -rf x",
        reason: "recursive delete",
        summary: "Deletes the x/ directory and everything inside it.",
      },
    ],
  };
  const items = buildTimeline(work);
  assert.ok(
    items[0].kind === "tool" && items[0].row.pending?.summary === "Deletes the x/ directory and everything inside it.",
  );
  assert.ok(items[0].kind === "tool" && items[0].row.pending?.reason === "recursive delete");
});

test("an approval whose tool_call isn't in the loaded activity lands at the end", () => {
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(1, "tool_call", { tool: "execute", command: "ls" }),
      act(2, "tool_result", { tool: "execute", code: 0 }),
    ],
    pendingApprovals: [{ requestId: "r9", command: "deploy prod", reason: "ship it" }],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 2, "the tool row plus a trailing fallback approval");
  assert.equal(items[1].kind, "approval");
  assert.ok(items[1].kind === "approval" && items[1].approval.requestId === "r9");
});

test("two distinct paused commands each get their own approval marker", () => {
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(1, "tool_call", { tool: "execute", command: "git clone a" }),
      act(2, "tool_result", { tool: "execute", blocked: "needs_approval", reason: "clone" }),
      act(3, "tool_call", { tool: "execute", command: "git clone b" }),
      act(4, "tool_result", { tool: "execute", blocked: "needs_approval", reason: "clone" }),
    ],
    pendingApprovals: [
      { requestId: "ra", command: "git clone a" },
      { requestId: "rb", command: "git clone b" },
    ],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 2, "two tool rows, each carrying its own marker — none left over at the end");
  assert.ok(items[0].kind === "tool" && items[0].row.pending?.requestId === "ra");
  assert.ok(items[1].kind === "tool" && items[1].row.pending?.requestId === "rb");
});

test("on a finished turn, retried identical orphan calls fold into one row with a count", () => {
  const work: WorkBlock = {
    status: "failed",
    activity: [
      act(1, "tool_call", { tool: "execute", command: "flaky" }),
      act(2, "tool_call", { tool: "execute", command: "flaky" }),
      act(3, "tool_call", { tool: "execute", command: "flaky" }),
    ],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 1);
  assert.ok(items[0].kind === "tool" && items[0].row.attempts === 3);
});

test("completed knowledge submit stays separate while identical skipped retries collapse", () => {
  const submit = {
    tool: "knowledge",
    arguments: { action: "submit", answer: "唐太宗。", candidate: "Emperor Taizong", question: "q" },
  };
  const skipped = {
    tool: "knowledge",
    isError: true,
    result: "[system] This identical tool action was skipped after repeated use.",
  };
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(1, "tool_call", { ...submit, callId: "submit-ok" }),
      act(2, "tool_result", { tool: "knowledge", callId: "submit-ok", isError: false }),
      act(3, "tool_call", { ...submit, callId: "submit-retry-1" }),
      act(4, "tool_result", { ...skipped, callId: "submit-retry-1" }),
      act(5, "tool_call", { ...submit, callId: "submit-retry-2" }),
      act(6, "tool_result", { ...skipped, callId: "submit-retry-2" }),
      act(7, "tool_call", { ...submit, callId: "submit-retry-3" }),
      act(8, "tool_result", { ...skipped, callId: "submit-retry-3" }),
    ],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 2);
  assert.ok(items[0]?.kind === "tool" && toolRowKind(items[0].row, work.status) === "ok");
  assert.ok(
    items[1]?.kind === "tool" &&
      toolRowKind(items[1].row, work.status) === "failed" &&
      items[1].row.attempts === 3,
  );
});

test("skipped retry labels support structured and legacy tool results", () => {
  assert.equal(
    toolFailureLabel("knowledge", { details: { errorCode: "duplicate_action_skipped" } }, "Tried project knowledge"),
    "Skipped repeated project knowledge",
  );
  assert.equal(
    toolFailureLabel(
      "knowledge",
      { result: "[system] This identical tool action was skipped after repeated use." },
      "Tried project knowledge",
    ),
    "Skipped repeated project knowledge",
  );
  assert.equal(
    toolFailureLabel("knowledge", { details: { errorCode: "post_validation_call_skipped" } }, "fallback"),
    "Skipped post-validation knowledge action",
  );
});

test("interleaved thinking breaks an orphan-call run so the rows aren't fused", () => {
  const work: WorkBlock = {
    status: "failed",
    activity: [
      act(1, "tool_call", { tool: "execute", command: "flaky" }),
      act(2, "thinking", { thinking: "hmm" }),
      act(3, "tool_call", { tool: "execute", command: "flaky" }),
    ],
  };
  const items = buildTimeline(work);
  assert.deepEqual(
    items.map((i) => i.kind),
    ["tool", "thinking", "tool"],
    "the thinking between the two identical calls keeps them as separate rows",
  );
});

test("interstitial narration (text) interleaves with thinking and tools by seq", () => {
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(1, "thinking", { thinking: "plan" }),
      act(2, "text", { text: "Let me check the config first." }),
      act(3, "tool_call", { tool: "read", path: "/cfg" }),
      act(4, "tool_result", { tool: "read" }),
    ],
  };
  const items = buildTimeline(work);
  assert.deepEqual(
    items.map((i) => i.kind),
    ["thinking", "text", "tool"],
    "narration sits between the thinking and the tool it precedes",
  );
  assert.ok(
    items[1].kind === "text" && (items[1].activity.payload as { text: string }).text.startsWith("Let me check"),
  );
});

test("phase events drive the live header without becoming fake tool rows", () => {
  const work: WorkBlock = {
    status: "working",
    activity: [
      act(1, "phase", { phase: "preparing" }),
      act(2, "tool_call", { tool: "knowledge", action: "read", callId: "knowledge-1" }),
      act(3, "phase", { phase: "using_tool", tool: "knowledge" }),
      act(4, "tool_result", { tool: "knowledge", action: "read", callId: "knowledge-1" }),
      act(5, "phase", { phase: "answering" }),
    ],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 1);
  assert.ok(items[0]?.kind === "tool" && items[0].row.call && items[0].row.result);
});

test("knowledge progress events attach to one tool row by callId", () => {
  const work: WorkBlock = {
    status: "working",
    activity: [
      act(1, "tool_call", { tool: "knowledge", action: "search", callId: "knowledge-1" }),
      act(2, "tool_progress", {
        tool: "knowledge",
        callId: "knowledge-1",
        code: "search_started",
        message: "Searching project knowledge",
      }),
      act(3, "tool_progress", {
        tool: "knowledge",
        callId: "knowledge-1",
        code: "search_complete",
        message: "Search complete",
      }),
      act(4, "tool_result", { tool: "knowledge", action: "search", callId: "knowledge-1", isError: true }),
    ],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 1);
  assert.ok(items[0]?.kind === "tool" && items[0].row.progress?.length === 2);
  assert.equal(
    items[0]?.kind === "tool" && (items[0].row.progress?.at(-1)?.payload as { code?: string }).code,
    "search_complete",
  );
});

test("results pair to their call by callId across batched (parallel) tool calls", () => {
  const work: WorkBlock = {
    status: "complete",
    activity: [
      act(3, "tool_call", { tool: "execute", command: "echo first", callId: "A" }),
      act(4, "tool_call", { tool: "execute", command: "echo second", callId: "B" }),
      act(5, "tool_call", { tool: "execute", command: "echo third", callId: "C" }),
      act(6, "tool_result", { tool: "execute", callId: "B", code: 0 }),
      act(9, "tool_call", { tool: "execute", command: "echo first", callId: "D" }),
      act(10, "tool_call", { tool: "execute", command: "echo third", callId: "E" }),
      act(11, "tool_result", { tool: "execute", callId: "D", code: 0 }),
      act(12, "tool_result", { tool: "execute", callId: "E", code: 0 }),
    ],
  };
  const items = buildTimeline(work);
  assert.equal(items.length, 5, "five tool rows, no stray orphan-result row");
  assert.ok(items.every((i) => i.kind === "tool"));
  const rows = items.map((i) => (i.kind === "tool" ? i.row : null));
  assert.ok(rows[0]!.call && !rows[0]!.result, "echo first (batch 1) stays resultless");
  assert.ok(
    rows[1]!.result && (rows[1]!.result.payload as { callId: string }).callId === "B",
    "echo second got result B, not C's",
  );
  assert.ok(rows[2]!.call && !rows[2]!.result, "echo third (batch 1) stays resultless");
  assert.ok(
    rows[3]!.result && (rows[3]!.result.payload as { callId: string }).callId === "D",
    "retried echo first got result D",
  );
  assert.ok(
    rows[4]!.result && (rows[4]!.result.payload as { callId: string }).callId === "E",
    "retried echo third got result E",
  );
});

test("toolRowKind: a successful result is `ok`", () => {
  assert.equal(
    toolRowKind(row({ tool: "publish", name: "site" }, { tool: "publish", url: "https://x" }), "complete"),
    "ok",
  );
  assert.equal(toolRowKind(row({ tool: "execute", command: "ls" }, { tool: "execute", code: 0 }), "complete"), "ok");
});

test("toolRowKind: an error result is `failed`, regardless of which tool", () => {
  assert.equal(
    toolRowKind(
      row({ tool: "publish", name: ":bad" }, { tool: "publish", error: "invalid deployment name" }),
      "complete",
    ),
    "failed",
  );
  assert.equal(
    toolRowKind(row({ tool: "write", path: "/x" }, { tool: "write", error: "EACCES" }), "complete"),
    "failed",
  );
});

test("toolRowKind: a knowledge tool is failed when its structured result has isError", () => {
  const knowledgeRow = row(
    { tool: "knowledge", action: "read", callId: "k1" },
    { tool: "knowledge", action: "read", callId: "k1", isError: true },
  );
  assert.equal(toolRowKind(knowledgeRow, "complete"), "failed");
});

test("toolRowKind: a policy-denied command is `failed`, and a nonzero exit is `failed`", () => {
  assert.equal(
    toolRowKind(
      row({ tool: "execute", command: "curl evil" }, { tool: "execute", denied: true, reason: "blocked host" }),
      "complete",
    ),
    "failed",
  );
  assert.equal(
    toolRowKind(row({ tool: "execute", command: "false" }, { tool: "execute", code: 1 }), "complete"),
    "failed",
  );
});

test("toolRowKind: a needs-approval result is `approval`", () => {
  assert.equal(
    toolRowKind(
      row(
        { tool: "execute", command: "rm -rf x" },
        { tool: "execute", blocked: "needs_approval", reason: "recursive delete" },
      ),
      "working",
    ),
    "approval",
  );
});

test("toolRowKind: no result yet — `running` mid-turn, `attempted`/`failed` once the turn ends", () => {
  assert.equal(toolRowKind(row({ tool: "publish", name: "site" }), "working"), "running", "mid-turn, still in flight");
  assert.equal(
    toolRowKind(row({ tool: "publish", name: "site" }), "complete"),
    "attempted",
    "turn finished cleanly but we never heard back — orphan, not a failure",
  );
  assert.equal(
    toolRowKind(row({ tool: "publish", name: "site" }), "failed"),
    "failed",
    "turn itself failed, so the in-flight call counts as failed",
  );
});

function workWith(activity: ToolActivity[], status: WorkBlock["status"] = "working"): WorkBlock {
  return { status, activity };
}

test("buildTimeline is memoized: unchanged (activity, status, approvals) returns the identical array", () => {
  const work = workWith([act(1, "tool_call", { tool: "execute", command: "ls", callId: "a" })]);
  const first = buildTimeline(work);
  assert.equal(buildTimeline(work), first, "same inputs must hit the memo");
});

test("the memo misses when activity is replaced, status flips, or approvals change", () => {
  const a1 = [act(1, "tool_call", { tool: "execute", command: "ls", callId: "a" })];
  const work = workWith(a1);
  const first = buildTimeline(work);

  work.activity = [...a1, act(2, "tool_result", { tool: "execute", callId: "a" })];
  const second = buildTimeline(work);
  assert.notEqual(second, first, "replaced activity must rebuild");
  assert.equal(buildTimeline(work), second, "and re-memoize");

  work.status = "complete";
  const third = buildTimeline(work);
  assert.notEqual(third, second, "a status change must rebuild (it drives collapsing)");

  work.pendingApprovals = [{ requestId: "r1", command: "rm -rf /", createdAt: 3 } as never];
  const fourth = buildTimeline(work);
  assert.notEqual(fourth, third, "changed approvals must rebuild");
});

test("memoized output still reflects a mutated-then-replaced work correctly", () => {
  const work = workWith([act(1, "thinking", { thinking: "hm" })], "working");
  assert.equal(buildTimeline(work).length, 1);
  work.activity = [
    act(1, "thinking", { thinking: "hm" }),
    act(2, "tool_call", { tool: "read", callId: "b", path: "x" }),
  ];
  const items = buildTimeline(work);
  assert.equal(items.length, 2);
  assert.equal(items[1]?.kind, "tool");
});
