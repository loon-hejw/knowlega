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
  JsonBlock,
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


export function TraceTable({ trace }: { trace: QueryAnswer["trace"] }) {
  const columns: ProColumns<NonNullable<QueryAnswer["trace"]>[number]>[] = [
    { title: "步骤", dataIndex: "step", width: 80 },
    { title: "动作", render: (_, item) => item.action.action, width: 130 },
    {
      title: "目标",
      render: (_, item) => item.action.path || item.action.query || item.action.rationale || "-",
      ellipsis: true,
    },
    { title: "观察结果", dataIndex: "observation", ellipsis: true },
  ];
  return <ProTable rowKey="step" columns={columns} dataSource={trace ?? []} pagination={false} search={false} options={false} toolBarRender={false} />;
}

export function EvidenceAudit({
  answer,
  onOpenFile,
}: {
  answer: QueryAnswer;
  onOpenFile?: (path: string) => void;
}) {
  const checks = new Map((answer.evidence_checks ?? []).map((check) => [check.requirement_id, check]));
  const rows = (answer.plan.requirements ?? []).map((requirement) => ({
    ...requirement,
    check: checks.get(requirement.id),
  }));
  const statusColor = (status: string | undefined, kind: string | undefined) => {
    if (status === "supported" || (status === "not_found_in_corpus" && kind === "negative")) return "green";
    if (status === "contradicted") return "red";
    return "orange";
  };
  const statusLabel = (status: string | undefined, kind: string | undefined) => {
    if (!status) return "未核验";
    if (status === "not_found_in_corpus") {
      return kind === "negative" ? "当前语料未发现反例" : "当前语料未找到证据";
    }
    return status;
  };
  return (
    <Space direction="vertical" className="page-stack" size="middle">
      {answer.candidate && <Alert type={answer.status === "complete" ? "success" : "info"} showIcon message={`${answer.status === "complete" ? "最终候选" : "最佳候选"}：${answer.candidate}`} />}
      {answer.incomplete_reason && <Alert type="warning" showIcon message="证据尚未闭合" description={answer.incomplete_reason} />}
      <Table
        rowKey="id"
        pagination={false}
        dataSource={rows}
        locale={{ emptyText: "该问题没有结构化要求。" }}
        columns={[
          { title: "条件", dataIndex: "id", width: 70 },
          { title: "要求", dataIndex: "text" },
          {
            title: "状态",
            width: 160,
            render: (_, row) => <Tag color={statusColor(row.check?.status, row.kind)}>{statusLabel(row.check?.status, row.kind)}</Tag>,
          },
          { title: "说明", render: (_, row) => row.check?.explanation ?? "-" },
          {
            title: "证据",
            render: (_, row) => (
              <Space direction="vertical" size={0}>
                {(row.check?.evidence_paths ?? []).map((path) =>
                  onOpenFile ? <Button key={path} type="link" className="path-link" onClick={() => onOpenFile(path)}>{path}</Button> : <span key={path}>{path}</span>,
                )}
              </Space>
            ),
          },
        ]}
      />
      {(answer.candidate_assessments ?? []).filter((item) => item.candidate !== answer.candidate).map((item) => (
        <Alert
          key={item.candidate}
          type={item.disposition === "rejected" ? "error" : "info"}
          showIcon
          message={`已核验候选：${item.candidate} · ${item.disposition}`}
          description={item.reason || [...(item.unresolved_requirement_ids ?? []), ...(item.contradicted_requirement_ids ?? [])].join("、")}
        />
      ))}
      {(answer.verification ?? []).map((verification) => (
        <Alert
          key={`${verification.pass}-${verification.kind}`}
          type={verification.accepted ? "success" : "error"}
          showIcon
          message={`验证 ${verification.pass} · ${verification.kind} · ${verification.accepted ? "通过" : "拒绝"}`}
          description={verification.summary || [...(verification.unresolved ?? []), ...(verification.contradictions ?? [])].join("；")}
        />
      ))}
    </Space>
  );
}

export function CitationsTable({
  citations,
  onOpenFile,
}: {
  citations: NonNullable<QueryAnswer["citations"]>;
  onOpenFile?: (path: string) => void;
}) {
  const columns: ProColumns<NonNullable<QueryAnswer["citations"]>[number]>[] = [
    {
      title: "路径",
      dataIndex: "path",
      ellipsis: true,
      render: (_, citation) =>
        onOpenFile ? (
          <Button type="link" className="path-link" onClick={() => onOpenFile(citation.path)}>
            {citation.path}
          </Button>
        ) : (
          citation.path
        ),
    },
    { title: "标题", dataIndex: "title", ellipsis: true },
    { title: "类型", dataIndex: "kind", width: 150 },
  ];
  return <ProTable rowKey="path" columns={columns} dataSource={citations} pagination={false} search={false} options={false} toolBarRender={false} />;
}

export function ResultsTable({
  results,
  onOpenFile,
}: {
  results: NonNullable<QueryAnswer["results"]>;
  onOpenFile?: (path: string) => void;
}) {
  const columns: ProColumns<NonNullable<QueryAnswer["results"]>[number]>[] = [
    { title: "标题", dataIndex: "title", ellipsis: true },
    {
      title: "路径",
      dataIndex: "path",
      ellipsis: true,
      render: (_, result) =>
        onOpenFile ? (
          <Button type="link" className="path-link" onClick={() => onOpenFile(result.path)}>
            {result.path}
          </Button>
        ) : (
          result.path
        ),
    },
    { title: "类型", dataIndex: "kind", width: 140 },
    {
      title: "条件覆盖",
      dataIndex: "matched_requirement_ids",
      width: 120,
      render: (_, result) => result.matched_requirement_ids?.length ? result.matched_requirement_ids.join(", ") : "-",
    },
    { title: "分数", dataIndex: "score", width: 100 },
    { title: "摘要", dataIndex: "snippet", ellipsis: true },
  ];
  return <ProTable rowKey="path" columns={columns} dataSource={results} search={false} options={false} toolBarRender={false} />;
}
