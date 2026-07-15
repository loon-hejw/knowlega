import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { QueryAnswer } from "../../types/api";
import { EvidenceAudit } from "./QueryTables";

function answerFixture(): QueryAnswer {
  return {
    question: "谁符合条件？",
    status: "incomplete",
    candidate: "镇元子",
    evidence_checks: [
      { requirement_id: "1", status: "supported", evidence_paths: ["wiki/entities/镇元子.md"] },
      { requirement_id: "7", status: "not_found_in_corpus", evidence_paths: ["wiki/entities/镇元子.md"] },
      { requirement_id: "9", status: "not_found_in_corpus", evidence_paths: ["wiki/entities/镇元子.md"] },
    ],
    candidate_assessments: [
      { candidate: "镇元子", disposition: "partial", unresolved_requirement_ids: ["7"], evidence_checks: [] },
      { candidate: "牛魔王", disposition: "rejected", contradicted_requirement_ids: ["5"], evidence_checks: [] },
    ],
    incomplete_reason: "未完成核验：7. 见过阎罗王",
    plan: {
      question: "谁符合条件？",
      intent: "answer_from_persistent_wiki",
      reasoning_mode: "constraint_satisfaction",
      requirements: [
        { id: "1", text: "有结义情节", kind: "positive" },
        { id: "7", text: "见过阎罗王", kind: "positive" },
        { id: "9", text: "不曾到过花果山", kind: "negative" },
      ],
      read_first: [],
      searches: [],
      candidate_limit: 10,
      answer_mode: "deep_constraint_verification",
      can_write_back: false,
    },
    results: [],
    answer: "当前没有完整匹配。",
    citations: [],
    trace: [{
      step: 99,
      action: {
        action: "final",
        candidate: "错误轨迹候选",
        evidence_checks: [{ requirement_id: "7", status: "supported" }],
      },
      observation: "rejected",
    }],
    notes: [],
  };
}

describe("EvidenceAudit", () => {
  it("renders the canonical answer ledger instead of the last trace action", () => {
    render(<EvidenceAudit answer={answerFixture()} />);
    expect(screen.getByText("最佳候选：镇元子")).toBeInTheDocument();
    expect(screen.getByText("未完成核验：7. 见过阎罗王")).toBeInTheDocument();
    expect(screen.getByText("当前语料未找到证据")).toBeInTheDocument();
    expect(screen.getByText("当前语料未发现反例")).toBeInTheDocument();
    expect(screen.queryByText("错误轨迹候选")).not.toBeInTheDocument();
    expect(screen.getByText("已核验候选：牛魔王 · rejected")).toBeInTheDocument();
  });
});
