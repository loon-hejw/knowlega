import { describe, expect, it } from "vitest";
import { activeRunStage, buildRunStages, eventTypeLabel } from "./QueryRun";

describe("unified query run progress", () => {
  it("shows context preparation followed by dynamic evidence work", () => {
    expect(activeRunStage("running", "context_started")).toBe("context");
    expect(activeRunStage("running", "strategy_ready")).toBe("evidence");

    const stages = buildRunStages("evidence", "running", "answer_from_persistent_wiki");
    expect(stages.map((stage) => stage.label)).toEqual([
      "接收问题",
      "准备上下文",
      "执行证据动作",
      "综合答案",
      "完成",
    ]);
    expect(stages.some((stage) => stage.label === "判断意图" || stage.label === "规划查询")).toBe(false);
    expect(eventTypeLabel("strategy_ready")).toBe("策略");
    expect(eventTypeLabel("action_failed")).toBe("动作失败");
    expect(eventTypeLabel("step_limit_reached")).toBe("步骤上限");
    expect(eventTypeLabel("candidate_discovery_started")).toBe("候选召回");
    expect(eventTypeLabel("candidate_audit_done")).toBe("核验完成");
    expect(eventTypeLabel("candidate_audit_batch_started")).toBe("核验批次");
    expect(eventTypeLabel("candidate_audit_batch_done")).toBe("批次完成");
    expect(eventTypeLabel("llm_output_normalized")).toBe("格式归一");
    expect(eventTypeLabel("query_incomplete")).toBe("证据未闭合");
  });

  it("uses the same first-turn lifecycle for non-wiki answers", () => {
    const stages = buildRunStages("evidence", "running", "direct_chat");
    expect(stages.map((stage) => stage.label)).toContain("执行回答动作");
    expect(stages.map((stage) => stage.label)).not.toContain("判断意图");
  });
});
