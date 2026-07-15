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
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ApiClient } from "../api/client";
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
} from "../types/api";

const { Text, Title, Paragraph } = Typography;


export function OperationOutput({ value }: { value: unknown }) {
  const response = value as Partial<IssuesResponse & ValidateWikiResult>;
  if (Array.isArray(response.issues)) {
    return (
      <div className="panel">
        <Table
          rowKey={(item) => `${item.Type}:${item.Path}:${item.Detail}`}
          columns={[
            { title: "类型", dataIndex: "Type", width: 160 },
            { title: "路径", dataIndex: "Path", width: 220, ellipsis: true },
            { title: "详情", dataIndex: "Detail" },
          ]}
          dataSource={response.issues}
        />
      </div>
    );
  }
  return <JsonBlock value={value} />;
}

export function JsonBlock({ value }: { value: unknown }) {
  return (
    <Collapse
      items={[
        {
          key: "json",
          label: "JSON",
          children: <pre className="json-block">{JSON.stringify(value, null, 2)}</pre>,
        },
      ]}
    />
  );
}

export function buildFileTree(files: ProjectFile[]): DataNode[] {
  type MutableNode = DataNode & { children?: MutableNode[] };
  const root: MutableNode[] = [];
  const dirs = new Map<string, MutableNode>();
  for (const file of files) {
    const parts = file.path.split("/");
    let prefix = "";
    let level = root;
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i];
      const key = prefix ? `${prefix}/${part}` : part;
      const isLeaf = i === parts.length - 1;
      if (isLeaf) {
        level.push({ key: file.path, title: part, isLeaf: true });
      } else {
        let node = dirs.get(key);
        if (!node) {
          node = { key, title: part, children: [] };
          dirs.set(key, node);
          level.push(node);
        }
        level = node.children ?? [];
      }
      prefix = key;
    }
  }
  return root;
}

export function buildSourceTree(files: ProjectFile[]): DataNode[] {
  type MutableNode = DataNode & { children?: MutableNode[] };
  const root: MutableNode = { key: "raw/sources", title: "raw/sources", children: [] };
  const dirs = new Map<string, MutableNode>([["raw/sources", root]]);
  for (const file of [...files].sort((a, b) => a.path.localeCompare(b.path))) {
    const rel = rawSourceRelativePath(file.path);
    if (!rel) continue;
    const parts = rel.split("/");
    let prefix = "raw/sources";
    let level = root.children ?? [];
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i];
      const key = `${prefix}/${part}`;
      const isLeaf = i === parts.length - 1;
      if (isLeaf) {
        level.push({ key: file.path, title: part, isLeaf: true });
      } else {
        let node = dirs.get(key);
        if (!node) {
          node = { key, title: part, children: [] };
          dirs.set(key, node);
          level.push(node);
        }
        level = node.children ?? [];
      }
      prefix = key;
    }
  }
  return [root];
}

export function rawSourceRelativePath(filePath: string): string {
  return filePath.replace(/^raw\/sources\/?/, "");
}

export function sourceTreeKeyForTargetDir(targetDir: string): string {
  const normalized = targetDir.trim().replace(/^\/+|\/+$/g, "");
  return normalized ? `raw/sources/${normalized}` : "raw/sources";
}

export function sourceTargetDirFromKey(key: string, rawSourcePathSet: Set<string>): string {
  let dirKey = key || "raw/sources";
  if (rawSourcePathSet.has(dirKey)) {
    const lastSlash = dirKey.lastIndexOf("/");
    dirKey = lastSlash > 0 ? dirKey.slice(0, lastSlash) : "raw/sources";
  }
  if (dirKey === "raw/sources") return "";
  return dirKey.startsWith("raw/sources/") ? dirKey.slice("raw/sources/".length) : "";
}

export function uploadRelativePath(file: File): string {
  const withDirectory = file as File & { webkitRelativePath?: string };
  return withDirectory.webkitRelativePath || file.name;
}

export function resolveProjectPath(input: string, files: ProjectFile[]): string {
  const value = normalizeWikiTarget(input);
  if (files.some((file) => file.path === value)) return value;
  const byPath = files.find((file) => file.path.toLowerCase() === value.toLowerCase());
  if (byPath) return byPath.path;
  const slug = slugify(value);
  const byBase = files.find((file) => slugify(file.path.replace(/\.md$/i, "").split("/").pop() ?? "") === slug);
  if (byBase) return byBase.path;
  const byTitlePath = files.find((file) => slugify(file.path.replace(/^wiki\//, "").replace(/\.md$/i, "")) === slug);
  return byTitlePath?.path ?? "";
}

export function normalizeWikiTarget(input: string): string {
  let value = input.trim().split("|")[0].split("#")[0];
  if (!value) return "";
  if (value.startsWith("raw/") || value === "purpose.md" || value === "schema.md") return value;
  if (value.startsWith("wiki/")) return value.endsWith(".md") ? value : `${value}.md`;
  if (value.includes("/") && value.endsWith(".md")) return value;
  if (value.includes("/") && !value.endsWith(".md")) return `${value}.md`;
  return value;
}

export function renderMarkdown(content: string, onOpenWikiLink: (path: string) => void): ReactNode[] {
  const lines = content.split("\n");
  const nodes: ReactNode[] = [];
  let inCode = false;
  let code: string[] = [];
  lines.forEach((line, index) => {
    if (line.startsWith("```")) {
      if (inCode) {
        nodes.push(
          <pre key={`code-${index}`} className="markdown-code">
            {code.join("\n")}
          </pre>,
        );
        code = [];
        inCode = false;
      } else {
        inCode = true;
      }
      return;
    }
    if (inCode) {
      code.push(line);
      return;
    }
    if (!line.trim()) {
      nodes.push(<div key={index} className="markdown-space" />);
      return;
    }
    if (line.startsWith("# ")) {
      nodes.push(<h1 key={index}>{renderInlineMarkdown(line.slice(2), onOpenWikiLink)}</h1>);
      return;
    }
    if (line.startsWith("## ")) {
      nodes.push(<h2 key={index}>{renderInlineMarkdown(line.slice(3), onOpenWikiLink)}</h2>);
      return;
    }
    if (line.startsWith("### ")) {
      nodes.push(<h3 key={index}>{renderInlineMarkdown(line.slice(4), onOpenWikiLink)}</h3>);
      return;
    }
    if (line.startsWith("- ")) {
      nodes.push(<li key={index}>{renderInlineMarkdown(line.slice(2), onOpenWikiLink)}</li>);
      return;
    }
    nodes.push(<p key={index}>{renderInlineMarkdown(line, onOpenWikiLink)}</p>);
  });
  if (code.length > 0) {
    nodes.push(
      <pre key="code-tail" className="markdown-code">
        {code.join("\n")}
      </pre>,
    );
  }
  return nodes;
}

export function renderInlineMarkdown(text: string, onOpenWikiLink: (path: string) => void): ReactNode[] {
  const nodes: ReactNode[] = [];
  const re = /\[\[([^\]]+)\]\]/g;
  let last = 0;
  let match: RegExpExecArray | null;
  while ((match = re.exec(text)) !== null) {
    if (match.index > last) nodes.push(text.slice(last, match.index));
    const raw = match[1];
    const [target, label] = raw.split("|");
    nodes.push(
      <Button key={`${target}-${match.index}`} type="link" className="inline-wikilink" onClick={() => onOpenWikiLink(target)}>
        {label || target}
      </Button>,
    );
    last = match.index + match[0].length;
  }
  if (last < text.length) nodes.push(text.slice(last));
  return nodes;
}

export function upsertServerSession(sessions: ChatSessionRecord[], next: ChatSessionRecord): ChatSessionRecord[] {
  const filtered = sessions.filter((session) => session.id !== next.id);
  return [next, ...filtered].sort((a, b) => b.updated_at.localeCompare(a.updated_at));
}

export function isScanSourcesResult(value: unknown): value is ScanSourcesResult {
  return Boolean(value && typeof value === "object" && "queued" in value && "skipped" in value);
}

export function truncateLabel(value: string, max: number): string {
  return value.length > max ? `${value.slice(0, max - 1)}...` : value;
}

export function slugify(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/\.md$/i, "")
    .replace(/[_\s]+/g, "-");
}

export function statusTag(status: string) {
  const color =
    status === "done" || status === "resolved" || status === "ok"
      ? "green"
      : status === "succeeded"
        ? "green"
      : status === "failed" || status === "dismissed"
        ? "red"
        : status === "processing" || status === "running"
          ? "blue"
          : status === "skipped"
            ? "default"
            : "gold";
  return <Tag color={color}>{statusLabel(status)}</Tag>;
}

export function statusLabel(status: string): string {
  switch (status) {
    case "pending":
      return "待处理";
    case "processing":
      return "处理中";
    case "queued":
      return "排队中";
    case "running":
      return "运行中";
    case "done":
      return "已完成";
    case "succeeded":
      return "成功";
    case "failed":
      return "失败";
    case "ok":
      return "成功";
    case "skipped":
      return "已跳过";
    case "open":
      return "未处理";
    case "resolved":
      return "已解决";
    case "dismissed":
      return "已忽略";
    case "added":
      return "新增";
    case "changed":
      return "变更";
    case "deleted":
      return "删除";
    case "unsupported":
      return "不支持";
    default:
      return status;
  }
}

export function maintainStepLabel(step: string): string {
  switch (step) {
    case "scan_sources":
      return "扫描来源";
    case "run_ingest_queue":
      return "运行摄取队列";
    case "structural_lint":
      return "结构检查";
    case "llm_review":
      return "LLM 审阅";
    case "sweep_reviews":
      return "清理审阅任务";
    case "sync_pg":
      return "同步 PG";
    default:
      return step;
  }
}

export function summaryText(summary?: Record<string, unknown>): string {
  if (!summary) {
    return "-";
  }
  return Object.entries(summary)
    .map(([key, value]) => `${key}=${String(value)}`)
    .join(" ");
}

export function canDeepResearch(review: ReviewItem): boolean {
  return ["missing-page", "source-gap", "review-needed", "suggestion"].includes(review.Type);
}

export function SeverityTag({ severity }: { severity: string }) {
  const value = severity.toLowerCase();
  const color =
    value === "high" || value === "critical"
      ? "red"
      : value === "medium"
        ? "orange"
        : value === "low"
          ? "blue"
          : "default";
  return <Tag color={color}>严重级别 {severity}</Tag>;
}

export function formatDateTime(value?: string | null): string {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
