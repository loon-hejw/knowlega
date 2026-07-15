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
  Avatar,
  Button,
  Col,
  Collapse,
  ConfigProvider,
  Descriptions,
  Drawer,
  Empty,
  Form,
  Input,
  InputNumber,
  List,
  Layout,
  Menu,
  Modal,
  Progress,
  Row,
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
import { ProCard } from "@ant-design/pro-components";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ApiClient } from "../../api/client";
import type { BrowserPageKey } from "../../app/navigation";
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
  GraphInsightsPanel,
  GraphPage,
  GraphRepositoriesPanel,
  ResearchJobsPanel,
} from "../../features/graph/GraphPage";
import { SettingsPage, TaskTable } from "./WikiAdminPage";
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


export function RepositoriesPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message } = AntApp.useApp();
  const [repositories, setRepositories] = useState<GraphRepositoryStatus[]>([]);
  const [jobs, setJobs] = useState<GraphIndexJob[]>([]);
  const [loading, setLoading] = useState(false);
  const refresh = useCallback(async () => { setLoading(true); try { const [repoData, jobData] = await Promise.all([api.graphRepositories(), api.graphJobs()]); setRepositories(repoData.repositories ?? []); setJobs(jobData.jobs ?? []); } catch (error) { message.error(errorMessage(error)); } finally { setLoading(false); } }, [api, message]);
  useEffect(() => { void refresh(); }, [refresh]);
  const queue = async (repo: GraphRepositoryStatus) => { try { await api.queueGraphJob({ registry_id: repo.registry_id, repository_id: repo.repository_id, branch: repo.branch }); message.success("索引任务已进入队列"); await refresh(); } catch (error) { message.error(errorMessage(error)); } };
  return <Space direction="vertical" size={16} className="page-stack"><div className="admin-page-heading"><div><Title level={2}>代码仓库</Title><Text type="secondary">以规范分支和 commit 固定代码证据</Text></div><Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>刷新</Button></div><div className="panel"><GraphRepositoriesPanel repositories={repositories} jobs={jobs} loading={loading} onQueue={(repo) => void queue(repo)} /></div></Space>;
}

export function AdminJobsPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message } = AntApp.useApp();
  const [workspaceJobs, setWorkspaceJobs] = useState<WorkspaceJob[]>([]);
  const [ingestTasks, setIngestTasks] = useState<IngestTask[]>([]);
  const [graphJobs, setGraphJobs] = useState<GraphIndexJob[]>([]);
  const [researchJobs, setResearchJobs] = useState<ResearchJob[]>([]);
  const [loading, setLoading] = useState(false);
  const refresh = useCallback(async () => { setLoading(true); const results = await Promise.allSettled([api.workspaceJobs(settings.projectPath), api.queueTasks(settings.projectPath), api.graphJobs(), api.researchJobs(settings.projectPath)]); if (results[0].status === "fulfilled") setWorkspaceJobs(results[0].value.jobs ?? []); if (results[1].status === "fulfilled") setIngestTasks(results[1].value.tasks ?? []); if (results[2].status === "fulfilled") setGraphJobs(results[2].value.jobs ?? []); if (results[3].status === "fulfilled") setResearchJobs(results[3].value.jobs ?? []); if (results.some((result) => result.status === "rejected")) message.warning("部分任务类型暂时不可用"); setLoading(false); }, [api, message, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
  return <Space direction="vertical" size={16} className="page-stack"><div className="admin-page-heading"><div><Title level={2}>任务中心</Title><Text type="secondary">统一观察导入、维护、代码索引与研究任务</Text></div><Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>刷新</Button></div><div className="panel"><Tabs items={[
    { key: "workspace", label: `维护 ${workspaceJobs.length}`, children: <Table rowKey="id" dataSource={workspaceJobs} columns={[{ title: "任务", dataIndex: "kind" }, { title: "状态", dataIndex: "status", render: (value) => statusTag(value) }, { title: "创建时间", dataIndex: "created_at", render: formatDateTime }, { title: "错误", dataIndex: "error", ellipsis: true }]} /> },
    { key: "ingest", label: `来源 ${ingestTasks.length}`, children: <TaskTable tasks={ingestTasks} /> },
    { key: "graph", label: `代码索引 ${graphJobs.length}`, children: <Table rowKey="id" dataSource={graphJobs} columns={[{ title: "仓库", dataIndex: "full_name" }, { title: "分支", dataIndex: "branch" }, { title: "状态", dataIndex: "status", render: (value) => statusTag(value) }, { title: "提交", dataIndex: "commit", render: (value) => value ? <Text code>{String(value).slice(0, 12)}</Text> : "-" }, { title: "错误", dataIndex: "error", ellipsis: true }]} /> },
    { key: "research", label: `研究 ${researchJobs.length}`, children: <ResearchJobsPanel jobs={researchJobs} onOpenFile={() => undefined} /> },
  ]} /></div></Space>;
}

export function QualityPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [issues, setIssues] = useState<IssuesResponse | null>(null);
  const [insights, setInsights] = useState<WikiGraphInsightsResponse | null>(null);
  const [status, setStatus] = useState<WorkspaceStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const refresh = useCallback(async () => { setLoading(true); try { const [lintData, insightData, statusData] = await Promise.all([api.lint(settings.projectPath), api.wikiGraphInsights(settings.projectPath), api.workspaceStatus({ project_path: settings.projectPath, project_id: settings.projectID, agent: settings.agent })]); setIssues(lintData); setInsights(insightData); setStatus(statusData); } catch (error) { message.error(errorMessage(error)); } finally { setLoading(false); } }, [api, message, settings.agent, settings.projectID, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
  return <Space direction="vertical" size={16} className="page-stack"><div className="admin-page-heading"><div><Title level={2}>质量治理</Title><Text type="secondary">结构健康、来源覆盖和知识维护信号</Text></div><Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>重新检查</Button></div><div className="quality-metrics"><Statistic title="结构问题" value={issues?.count ?? 0} /><Statistic title="待审阅" value={status?.reviews.open ?? 0} /><Statistic title="孤立页面" value={insights?.isolated_pages.length ?? 0} /><Statistic title="缺少来源" value={insights?.missing_sources.length ?? 0} /></div><div className="quality-layout"><div className="panel"><Title level={4}>结构问题</Title><Table rowKey={(issue) => `${issue.Type}:${issue.Path}:${issue.Detail}`} size="small" dataSource={issues?.issues ?? []} columns={[{ title: "类型", dataIndex: "Type", width: 140, render: (value) => <Tag color="orange">{value}</Tag> }, { title: "路径", dataIndex: "Path", render: (value) => <Button type="link" className="path-link" onClick={() => onOpenFile(value)}>{value}</Button> }, { title: "详情", dataIndex: "Detail" }]} /></div><div className="panel"><Title level={4}>图谱洞察</Title><GraphInsightsPanel insights={insights} onOpenFile={onOpenFile} onResearch={() => message.info("请在审阅中心发起研究任务")} /></div></div></Space>;
}

export function SystemDiagnosticsPage({ api, settings, updateSettings }: { api: ApiClient; settings: AppSettings; updateSettings: (patch: Partial<AppSettings>) => void }) {
  const { message } = AntApp.useApp();
  const [health, setHealth] = useState<HealthResponse | null>(null);
  const [status, setStatus] = useState<WorkspaceStatus | null>(null);
  const refresh = useCallback(async () => { try { const [healthData, statusData] = await Promise.all([api.health(), api.workspaceStatus({ project_path: settings.projectPath, project_id: settings.projectID, agent: settings.agent })]); setHealth(healthData); setStatus(statusData); message.success("诊断信息已刷新"); } catch (error) { message.error(errorMessage(error)); } }, [api, message, settings.agent, settings.projectID, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
  return <Space direction="vertical" size={16} className="page-stack"><div className="admin-page-heading"><div><Title level={2}>系统诊断</Title><Text type="secondary">服务、存储与模型能力状态</Text></div><Button icon={<ReloadOutlined />} onClick={refresh}>连接测试</Button></div><div className="diagnostic-grid">{[
    ["API 服务", health?.ok, health?.ready === false ? "正在初始化" : "可用"],
    ["PostgreSQL", status?.pg_configured, status?.pg_configured ? "已配置" : "未配置"],
    ["Embedding", status?.embedding_configured, status?.embedding_configured ? "已配置" : "未配置"],
    ["LLM 智能体", status?.agent === "llm", status?.agent || "llm"],
  ].map(([title, ok, detail]) => <section key={String(title)}><span className={ok ? "diagnostic-dot ok" : "diagnostic-dot"} /><div><Text strong>{String(title)}</Text><Text type="secondary">{String(detail)}</Text></div>{ok ? <Tag color="green">正常</Tag> : <Tag>检查配置</Tag>}</section>)}</div><SettingsPage settings={settings} updateSettings={updateSettings} /></Space>;
}
