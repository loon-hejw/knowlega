import {
  ApiOutlined,
  BellOutlined,
  BookOutlined,
  CalendarOutlined,
  CheckCircleOutlined,
  ClusterOutlined,
  CodeOutlined,
  CopyOutlined,
  DatabaseOutlined,
  EditOutlined,
  EyeOutlined,
  FileMarkdownOutlined,
  FileSearchOutlined,
  FolderOpenOutlined,
  ForkOutlined,
  HomeOutlined,
  InboxOutlined,
  MessageOutlined,
  NodeIndexOutlined,
  PlayCircleOutlined,
  ReloadOutlined,
  RightOutlined,
  RollbackOutlined,
  SaveOutlined,
  SearchOutlined,
  SendOutlined,
  SettingOutlined,
  SyncOutlined,
  ToolOutlined,
  UploadOutlined,
  WarningOutlined,
} from "@ant-design/icons";
import {
  Alert,
  App as AntApp,
  Button,
  Collapse,
  ConfigProvider,
  Descriptions,
  Drawer,
  Empty,
  Form,
  Input,
  InputNumber,
  Layout,
  Menu,
  Modal,
  Progress,
  Select,
  Space,
  Spin,
  Statistic,
  Switch,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Tree,
  Typography,
} from "antd";
import type { MenuProps, TableColumnsType } from "antd";
import type { DataNode } from "antd/es/tree";
import type { EdgeData, Graph as G6Graph, IPointerEvent, NodeData } from "@antv/g6";
import { ProTable } from "@ant-design/pro-components";
import type { ProColumns } from "@ant-design/pro-components";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ApiClient } from "../../api/client";
import type {
  AgentName,
  AppSettings,
  BootstrapStatus,
  ChatMessageRecord,
  ChatRunEvent,
  ChatSessionRecord,
  DeleteSourceResult,
  GraphIndexJob,
  GraphRepositoryStatus,
  HealthResponse,
  IngestTask,
  IssuesResponse,
  MaintainWikiResult,
  ProjectFile,
  ProjectFileContent,
  QueryAnswer,
  ReviewItem,
  ResearchJob,
  ReviewsResponse,
  RunQueueResult,
  ScanSourcesResult,
  SourceManifestEntry,
  UploadSourcesResponse,
  ValidateWikiResult,
  WikiGraphInsight,
  WikiGraphInsightsResponse,
  WikiGraphResponse,
  WorkspaceJob,
  WorkspaceJobsResponse,
  WorkspaceStatus,
} from "../../types/api";


import {
  OperationOutput,
  SeverityTag,
  buildFileTree,
  buildSourceTree,
  canDeepResearch,
  errorMessage,
  formatDateTime,
  isScanSourcesResult,
  maintainStepLabel,
  normalizeWikiTarget,
  rawSourceRelativePath,
  renderMarkdown,
  resolveProjectPath,
  slugify,
  sourceTargetDirFromKey,
  sourceTreeKeyForTargetDir,
  statusLabel,
  statusTag,
  summaryText,
  truncateLabel,
  uploadRelativePath,
  upsertServerSession,
} from "../../shared/utils";

const { Text, Title, Paragraph } = Typography;


import { CitationsTable, EvidenceAudit, ResultsTable, TraceTable } from "./QueryTables";
export type ActiveChatRunState = {
  chatID: string;
  runID: string;
  question: string;
  status: "running" | "succeeded" | "failed" | "canceled";
  startedAt: string;
  events: ChatRunEvent[];
};

export function ChatRunBubble({
  run,
  now,
  onCancel,
}: {
  run: ActiveChatRunState;
  now: number;
  onCancel: () => void;
}) {
  const summary = summarizeChatRun(run, now);
  const detailEvents = run.events.filter((event) => event.type !== "heartbeat").slice(-12);
  return (
    <div className="chat-bubble assistant running">
      <div className="run-card-header">
        <Space direction="vertical" size={4}>
          <Space wrap>
            <Text type="secondary">Knowledge Core</Text>
            {statusTag(run.status)}
            <Tag color={summary.live ? "blue" : "default"}>{summary.live ? "连接正常" : "等待事件"}</Tag>
            {summary.intent && <Tag>{intentLabel(summary.intent)}</Tag>}
          </Space>
          <Text strong>{summary.title}</Text>
          <Text type="secondary">{summary.subtitle}</Text>
        </Space>
        {run.status === "running" && (
          <Button danger size="small" onClick={onCancel}>
            取消
          </Button>
        )}
      </div>

      <div className="run-stage-grid">
        {summary.stages.map((stage, index) => (
          <div key={stage.key} className={`run-stage ${stage.status}`}>
            <span className="run-stage-index">{index + 1}</span>
            <span>{stage.label}</span>
          </div>
        ))}
      </div>
      <Progress percent={summary.percent} showInfo={false} status={run.status === "failed" ? "exception" : undefined} />

      {summary.showEvidence && (
        <div className="run-evidence-grid">
          <div>
            <Text strong>{summary.reads}</Text>
            <Text type="secondary">读取</Text>
          </div>
          <div>
            <Text strong>{summary.searches}</Text>
            <Text type="secondary">搜索</Text>
          </div>
          <div>
            <Text strong>{summary.graphs}</Text>
            <Text type="secondary">图谱</Text>
          </div>
          <div>
            <Text strong>{summary.citations}</Text>
            <Text type="secondary">引用</Text>
          </div>
        </div>
      )}

      {summary.observation && <Paragraph className="run-observation">{summary.observation}</Paragraph>}

      <Collapse
        size="small"
        ghost
        items={[
          {
            key: "details",
            label: `执行详情 · ${detailEvents.length} 条事件`,
            children: (
              <Space direction="vertical" size={6} className="page-stack">
                {detailEvents.map((event) => (
                  <div key={event.id} className="run-event-row">
                    <Tag>{eventTypeLabel(event.type)}</Tag>
                    {event.step ? <Text type="secondary">step {event.step}</Text> : null}
                    {event.action?.action ? <Text code>{event.action.action}</Text> : null}
                    <Text>{event.message}</Text>
                    {event.elapsed_ms ? <Text type="secondary">{formatElapsed(event.elapsed_ms)}</Text> : null}
                  </div>
                ))}
                {detailEvents.length === 0 && <Text type="secondary">等待第一条执行事件。</Text>}
              </Space>
            ),
          },
        ]}
      />
    </div>
  );
}

export function ChatBubble({
  message,
  onOpenFile,
}: {
  message: ChatMessageRecord;
  onOpenFile: (path: string) => void;
}) {
  const answer = message.answer;
  return (
    <div className={`chat-bubble ${message.role}`}>
      <Text type="secondary">{message.role === "user" ? "你" : "Knowledge Core"}</Text>
      <Paragraph className="answer-block">{message.content}</Paragraph>
      {answer && (
        <>
          {isNonWikiRunIntent(answer.plan.intent) && (
            <Alert
              showIcon
              type="info"
              message="此回答未引用知识库证据"
              description="它不会写回 wiki；需要业务、项目或资料依据的问题会进入知识库证据流程。"
            />
          )}
          <Tabs
            size="small"
            items={[
              {
                key: "evidence-audit",
                label: "证据核验",
                children: <EvidenceAudit answer={answer} onOpenFile={onOpenFile} />,
              },
              {
                key: "citations",
                label: "引用",
                children: <CitationsTable citations={answer.citations ?? []} onOpenFile={onOpenFile} />,
              },
              {
                key: "trace",
                label: "执行轨迹",
                children: <TraceTable trace={answer.trace ?? []} />,
              },
              {
                key: "results",
                label: "结果",
                children: <ResultsTable results={answer.results ?? []} onOpenFile={onOpenFile} />,
              },
            ]}
          />
        </>
      )}
      {message.writeback && (
        <Button type="link" className="path-link" onClick={() => onOpenFile(message.writeback!.path)}>
          打开综合页：{message.writeback.path}
        </Button>
      )}
    </div>
  );
}

export function upsertRunEvent(events: ChatRunEvent[], event: ChatRunEvent): ChatRunEvent[] {
  if (events.some((item) => item.id === event.id)) return events;
  return [...events, event];
}

export type RunStageStatus = "waiting" | "active" | "done" | "failed" | "canceled";

export type RunStageSummary = {
  key: string;
  label: string;
  status: RunStageStatus;
};

export type RunSummary = {
  title: string;
  subtitle: string;
  observation: string;
  percent: number;
  live: boolean;
  intent: string;
  reads: number;
  searches: number;
  graphs: number;
  citations: number;
  showEvidence: boolean;
  stages: RunStageSummary[];
};

export function summarizeChatRun(run: ActiveChatRunState, now: number): RunSummary {
  const meaningful = run.events.filter((event) => event.type !== "heartbeat");
  const latest = meaningful[meaningful.length - 1];
  const latestAny = run.events[run.events.length - 1];
  const elapsedMs = run.status === "running" ? Math.max(0, now - new Date(run.startedAt).getTime()) : (latestAny?.elapsed_ms ?? 0);
  const activeKey = activeRunStage(run.status, latest?.type);
  const completed = run.status === "succeeded";
  const failed = run.status === "failed";
  const canceled = run.status === "canceled";
  const citations = latestAny?.answer?.citations?.length ?? 0;
  const intent = latestAny?.answer?.plan?.intent ?? routeIntentFromEvents(meaningful);
  const showEvidence = !isNonWikiRunIntent(intent);
  return {
    title: runTitle(run.status, activeKey, latest),
    subtitle: runSubtitle(run.status, elapsedMs, latest),
    observation: latest?.observation ?? "",
    percent: completed ? 100 : failed || canceled ? stagePercent(activeKey) : Math.min(stagePercent(activeKey), 92),
    live: Boolean(latestAny) && elapsedMs - (latestAny.elapsed_ms ?? 0) < 8000,
    intent,
    reads: countRunActions(meaningful, ["read", "follow_links"]),
    searches: countRunActions(meaningful, ["search"]),
    graphs: countRunActions(meaningful, ["graph", "expand"]),
    citations,
    showEvidence,
    stages: buildRunStages(activeKey, run.status, intent),
  };
}

export function activeRunStage(status: ActiveChatRunState["status"], latestType?: string): string {
  if (status === "succeeded") return "done";
  if (status === "failed") return "done";
  if (status === "canceled") return "done";
  if (!latestType || latestType === "started") return "intake";
  if (latestType === "context_started" || latestType === "context_ready") return "context";
  if (latestType === "strategy_ready") return "evidence";
  // Historical runs may still contain the former router/planner events.
  if (latestType === "routing_started" || latestType === "routing_done") return "routing";
  if (latestType === "general_answer_started" || latestType === "general_answer_done") return "direct_answer";
  if (latestType === "planning_started" || latestType === "planning_done") return "planning";
  if (latestType === "synthesis_started") return "synthesis";
  if (latestType === "completed" || latestType === "error" || latestType === "canceled") return "done";
  return "evidence";
}

export function buildRunStages(activeKey: string, status: ActiveChatRunState["status"], intent?: string): RunStageSummary[] {
  const order = [
    { key: "intake", label: "接收问题" },
    { key: "context", label: "准备上下文" },
    { key: "evidence", label: isNonWikiRunIntent(intent) ? "执行回答动作" : "执行证据动作" },
    { key: "synthesis", label: "综合答案" },
    { key: "done", label: "完成" },
  ];
  const activeIndex = order.findIndex((stage) => stage.key === activeKey);
  return order.map((stage, index) => {
    let stageStatus: RunStageStatus = index < activeIndex ? "done" : index === activeIndex ? "active" : "waiting";
    if (status === "succeeded") stageStatus = "done";
    if ((status === "failed" || status === "canceled") && index === activeIndex) stageStatus = status;
    return { ...stage, status: stageStatus };
  });
}

export function runTitle(status: ActiveChatRunState["status"], stage: string, latest?: ChatRunEvent): string {
  if (status === "succeeded") return "答案已生成";
  if (status === "failed") return latest?.message || "查询失败";
  if (status === "canceled") return "查询已取消";
  if (stage === "context") return "正在准备知识库上下文";
  if (stage === "routing") return "正在判断问题意图";
  if (stage === "direct_answer") return "正在直接生成回答";
  if (stage === "planning") return "正在等待模型制定查询计划";
  if (stage === "evidence") return latest?.action?.action ? `正在执行 ${actionLabel(latest.action.action)}` : "正在收集证据";
  if (stage === "synthesis") return "正在等待模型综合答案";
  return "已收到问题，准备开始查询";
}

export function runSubtitle(status: ActiveChatRunState["status"], elapsedMs: number, latest?: ChatRunEvent): string {
  if (status === "running") {
    const waiting = latest ? `当前：${eventTypeLabel(latest.type)}` : "等待第一条事件";
    return `${waiting} · 已等待 ${formatElapsed(elapsedMs)}`;
  }
  if (status === "succeeded") return `总耗时 ${formatElapsed(elapsedMs)} · 执行过程已折叠`;
  if (status === "canceled") return `在 ${formatElapsed(elapsedMs)} 后取消，未写入答案`;
  return `在 ${formatElapsed(elapsedMs)} 后结束`;
}

export function stagePercent(stage: string): number {
  const values: Record<string, number> = {
    intake: 12,
    context: 28,
    routing: 26,
    planning: 42,
    evidence: 68,
    synthesis: 88,
    direct_answer: 78,
    done: 100,
  };
  return values[stage] ?? 12;
}

export function routeIntentFromEvents(events: ChatRunEvent[]): string {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const match = events[i].observation?.match(/intent=([a-z_]+)/);
    if (match?.[1]) return match[1];
  }
  return "";
}

export function isNonWikiRunIntent(intent?: string): boolean {
  return ["direct_chat", "system_faq", "general_assistant", "unsupported"].includes(intent ?? "");
}

export function countRunActions(events: ChatRunEvent[], actions: string[]): number {
  return events.filter((event) => event.type === "action_done" && event.action?.action && actions.includes(event.action.action)).length;
}

export function actionLabel(action: string): string {
  const labels: Record<string, string> = {
    read: "读取页面",
    list_pages: "浏览目录",
    list: "浏览目录",
    follow_links: "展开链接",
    search: "搜索页面",
    graph: "查询图谱",
    expand: "查询图谱",
    assess: "核验候选",
    assess_candidate: "核验候选",
    final: "生成答案",
    writeback: "准备写回",
  };
  return labels[action] ?? action;
}

export function formatElapsed(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return `${minutes}m ${rest}s`;
}

export function terminalChatRunStatus(type: string): ActiveChatRunState["status"] | null {
  if (type === "completed") return "succeeded";
  if (type === "error") return "failed";
  if (type === "canceled") return "canceled";
  return null;
}

export function eventTypeLabel(type: string): string {
  const labels: Record<string, string> = {
    started: "启动",
    context_started: "上下文",
    context_ready: "上下文就绪",
    strategy_ready: "策略",
    routing_started: "意图",
    routing_done: "路由",
    planning_started: "规划",
    planning_done: "计划",
    read_started: "读取",
    action_planning_started: "决策",
    action_started: "动作",
    action_done: "完成",
    search_started: "搜索",
    search_done: "召回",
    synthesis_started: "综合",
    general_answer_started: "直答",
    general_answer_done: "直答完成",
    heartbeat: "心跳",
    llm_retrying: "重试",
    llm_output_normalized: "格式归一",
    candidate_discovery_started: "候选召回",
    candidate_discovery_done: "候选完成",
    candidate_audit_started: "候选核验",
    candidate_audit_batch_started: "核验批次",
    candidate_audit_batch_done: "批次完成",
    candidate_audit_done: "核验完成",
    candidate_assessed: "候选账本",
    query_incomplete: "证据未闭合",
    action_failed: "动作失败",
    step_limit_reached: "步骤上限",
    writeback_done: "写回",
    completed: "完成",
    error: "失败",
    canceled: "取消",
  };
  return labels[type] ?? type;
}

export function intentLabel(intent: string): string {
  const labels: Record<string, string> = {
    direct_chat: "简单对话",
    system_faq: "系统问题",
    general_assistant: "普通问答",
    missing_evidence: "证据不足",
    unsupported: "不支持",
    wiki_query: "知识库",
    answer_from_persistent_wiki: "知识库",
    offline_fallback_query: "离线检索",
  };
  return labels[intent] ?? intent;
}

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}
