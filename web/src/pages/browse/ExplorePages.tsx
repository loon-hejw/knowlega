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
  GraphPage,
  UnifiedGraphCanvas,
  domainColor,
} from "../../features/graph/GraphPage";
import { formatHomeDate, knowledgeTitle } from "./HomePage";
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


export function KnowledgeDiscoveryPage({
  api,
  settings,
  onOpenFile,
  initialQuery = "",
}: {
  api: ApiClient;
  settings: AppSettings;
  onOpenFile: (path: string) => void;
  initialQuery?: string;
}) {
  const { message } = AntApp.useApp();
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [query, setQuery] = useState(initialQuery);
  const [loading, setLoading] = useState(false);
  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setGraph(await api.projectGraph({ project_path: settings.projectPath, query, domains: ["wiki", "source"], limit: 300 }));
    } catch (error) { message.error(errorMessage(error)); }
    finally { setLoading(false); }
  }, [api, message, query, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
  useEffect(() => { setQuery(initialQuery); }, [initialQuery]);
  const groups = useMemo(() => {
    const values = new Map<string, WikiGraphResponse["nodes"]>();
    for (const node of graph?.nodes ?? []) {
      const key = node.kind || node.type || "其他";
      values.set(key, [...(values.get(key) ?? []), node]);
    }
    return Array.from(values.entries()).sort((a, b) => b[1].length - a[1].length);
  }, [graph]);
  return (
    <Space direction="vertical" size={18} className="page-stack browse-content-page">
      <div className="browse-page-heading"><div><Title level={2}>主题与实体</Title><Text type="secondary">从 Wiki、来源和关系中自动形成的知识入口</Text></div><Input.Search value={query} onChange={(event) => setQuery(event.target.value)} onSearch={() => void refresh()} loading={loading} placeholder="搜索主题或实体" /></div>
      <div className="topic-grid">
        {groups.map(([kind, nodes]) => (
          <section key={kind} className="topic-section">
            <div className="section-heading-inline"><Title level={4}>{kind}</Title><Tag>{nodes.length}</Tag></div>
            {nodes.slice(0, 8).map((node) => (
              <button key={node.id} onClick={() => node.path && onOpenFile(node.path)} disabled={!node.path}>
                <ForkOutlined /><span><strong>{node.label || node.title || node.id}</strong><small>{node.path || node.source_ref || node.domain}</small></span><Text type="secondary">{node.in_degree + node.out_degree} 关系</Text>
              </button>
            ))}
          </section>
        ))}
        {groups.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无主题与实体，知识图谱将在来源处理后形成" />}
      </div>
    </Space>
  );
}

export function CodeKnowledgePage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [repositories, setRepositories] = useState<GraphRepositoryStatus[]>([]);
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [loading, setLoading] = useState(false);
  useEffect(() => {
    setLoading(true);
    void Promise.all([api.graphRepositories(), api.projectGraph({ project_path: settings.projectPath, domains: ["code"], limit: 300 })])
      .then(([repos, graphData]) => { setRepositories(repos.repositories ?? []); setGraph(graphData); })
      .catch((error) => message.error(errorMessage(error)))
      .finally(() => setLoading(false));
  }, [api, message, settings.projectPath]);
  return (
    <Space direction="vertical" size={18} className="page-stack browse-content-page">
      <div className="browse-page-heading"><div><Title level={2}>系统与代码</Title><Text type="secondary">跨仓库浏览系统、模块和精确代码事实</Text></div><Tag color="cyan">{repositories.length} 个仓库</Tag></div>
      <div className="code-knowledge-layout">
        <section className="repo-browser-list">
          <Title level={4}>代码仓库</Title>
          {repositories.map((repo) => <div key={`${repo.registry_id}:${repo.repository_id}`}><CodeOutlined /><span><strong>{repo.full_name}</strong><small>{repo.provider} · {repo.branch}{repo.commit ? ` · ${repo.commit.slice(0, 12)}` : ""}</small></span><Tag color={repo.indexed ? "green" : "default"}>{repo.indexed ? "已索引" : "未索引"}</Tag></div>)}
          {repositories.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无代码仓库" />}
        </section>
        <section className="symbol-browser-list">
          <div className="section-heading-inline"><Title level={4}>关键符号与模块</Title><Text type="secondary">{graph?.nodes?.length ?? 0} 个节点</Text></div>
          <Table loading={loading} rowKey="id" size="small" pagination={{ pageSize: 12 }} dataSource={graph?.nodes ?? []} columns={[
            { title: "名称", render: (_, node) => <Button type="link" className="path-link" disabled={!node.path} onClick={() => node.path && onOpenFile(node.path)}>{node.label || node.title || node.id}</Button> },
            { title: "类型", dataIndex: "kind", width: 130, render: (value) => <Tag>{value}</Tag> },
            { title: "范围", dataIndex: "scope_id", width: 180, ellipsis: true },
            { title: "关系", width: 90, render: (_, node) => node.in_degree + node.out_degree },
          ]} />
        </section>
      </div>
    </Space>
  );
}

export function CollectionsPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [files, setFiles] = useState<ProjectFile[]>([]);
  useEffect(() => { void api.projectFiles(settings.projectPath).then((data) => setFiles(data.files ?? [])).catch((error) => message.error(errorMessage(error))); }, [api, message, settings.projectPath]);
  const collections = [
    { title: "最近更新", description: "最近演进的知识页面", paths: files.filter((file) => file.path.startsWith("wiki/")).sort((a, b) => (b.mod_time || "").localeCompare(a.mod_time || "")).slice(0, 8) },
    { title: "代码知识", description: "系统、模块、社区与流程", paths: files.filter((file) => file.path.startsWith("wiki/code/")) },
    { title: "综合结论", description: "可复用的跨来源综合", paths: files.filter((file) => file.path.startsWith("wiki/syntheses/")) },
    { title: "概念与实体", description: "知识库自动形成的语义入口", paths: files.filter((file) => file.path.startsWith("wiki/concepts/") || file.path.startsWith("wiki/entities/")) },
  ];
  return <Space direction="vertical" size={18} className="page-stack browse-content-page"><div className="browse-page-heading"><div><Title level={2}>知识集合</Title><Text type="secondary">由当前 Wiki 动态形成的精选视图</Text></div></div><div className="collection-grid">{collections.map((collection) => <section key={collection.title}><BookOutlined /><div><Title level={4}>{collection.title}</Title><Text type="secondary">{collection.description}</Text></div><Tag>{collection.paths.length}</Tag><div className="collection-items">{collection.paths.slice(0, 5).map((file) => <Button key={file.path} type="link" onClick={() => onOpenFile(file.path)}>{knowledgeTitle(file.path)}</Button>)}</div></section>)}</div></Space>;
}

export function SourceLibraryPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [sources, setSources] = useState<SourceManifestEntry[]>([]);
  const [files, setFiles] = useState<ProjectFile[]>([]);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [sourceData, fileData] = await Promise.all([api.sources(settings.projectPath), api.projectFiles(settings.projectPath)]);
      setSources(sourceData.sources ?? []);
      setFiles(fileData.files ?? []);
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  }, [api, message, settings.projectPath]);

  useEffect(() => { void refresh(); }, [refresh]);

  const rawFiles = useMemo(() => files.filter((file) => file.kind === "raw-source"), [files]);
  const visibleSources = useMemo(() => {
    const value = query.trim().toLowerCase();
    if (!value) return sources;
    return sources.filter((source) => [source.title, source.raw_path, source.original_path].some((field) => field?.toLowerCase().includes(value)));
  }, [query, sources]);
  const visibleFiles = useMemo(() => {
    const value = query.trim().toLowerCase();
    if (!value) return rawFiles;
    return rawFiles.filter((file) => file.path.toLowerCase().includes(value));
  }, [query, rawFiles]);

  return (
    <Space direction="vertical" size={22} className="page-stack browse-content-page source-library-page">
      <div className="browse-page-heading">
        <div><Title level={2}>来源与文档</Title><Text type="secondary">浏览知识库使用的原始证据，导入与维护操作位于管理端</Text></div>
        <Input.Search value={query} onChange={(event) => setQuery(event.target.value)} allowClear placeholder="搜索来源标题或路径" />
      </div>
      <div className="source-library-summary">
        <span><strong>{sources.length}</strong><small>已归档来源</small></span>
        <span><strong>{rawFiles.length}</strong><small>原始证据文件</small></span>
      </div>
      <div className="source-library-layout" aria-busy={loading}>
        <section>
          <div className="home-section-heading"><Title level={3}>来源清单</Title></div>
          <div className="home-list">
            {visibleSources.map((source) => (
              <button key={source.key} className="home-list-row" onClick={() => onOpenFile(source.content_path || source.raw_path)}>
                <span className="home-row-icon source"><FileSearchOutlined /></span>
                <span className="home-row-copy"><strong>{source.title || knowledgeTitle(source.raw_path)}</strong><small>{source.original_path || source.raw_path}</small></span>
                <time>{formatHomeDate(source.updated_at)}</time>
              </button>
            ))}
            {visibleSources.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="未找到来源" />}
          </div>
        </section>
        <section>
          <div className="home-section-heading"><Title level={3}>原始文件</Title></div>
          <div className="home-list">
            {visibleFiles.slice(0, 40).map((file) => (
              <button key={file.path} className="home-list-row" onClick={() => onOpenFile(file.path)}>
                <span className="home-row-icon source"><FileMarkdownOutlined /></span>
                <span className="home-row-copy"><strong>{knowledgeTitle(file.path)}</strong><small>{file.path}</small></span>
                <time>{formatHomeDate(file.mod_time)}</time>
              </button>
            ))}
            {visibleFiles.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="未找到原始文件" />}
          </div>
        </section>
      </div>
    </Space>
  );
}

export function BrowseGraphPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [selected, setSelected] = useState<WikiGraphResponse["nodes"][number] | null>(null);
  const [query, setQuery] = useState("");
  const refresh = useCallback(async () => { try { setGraph(await api.projectGraph({ project_path: settings.projectPath, query, limit: 350 })); } catch (error) { message.error(errorMessage(error)); } }, [api, message, query, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
  const selectNode = async (id: string) => { try { setSelected((await api.graphNode(settings.projectPath, id)).node); } catch (error) { message.error(errorMessage(error)); } };
  return <Space direction="vertical" size={16} className="page-stack browse-content-page"><div className="browse-page-heading"><div><Title level={2}>知识图谱</Title><Text type="secondary">探索来源、Wiki、主题、系统与代码之间的证据关系</Text></div><Input.Search value={query} onChange={(event) => setQuery(event.target.value)} onSearch={() => void refresh()} placeholder="搜索节点或关系" /></div><div className="browse-graph-layout"><div className="graph-canvas"><UnifiedGraphCanvas graph={graph} selectedNodeID={selected?.id ?? null} onSelectNode={(id) => void selectNode(id)} /></div><aside className="graph-detail-panel">{selected ? <Space direction="vertical" size={12} className="page-stack"><Tag color={domainColor(selected.domain)}>{selected.domain}</Tag><Title level={3}>{selected.label || selected.title}</Title><Text type="secondary">{selected.kind}</Text><Text code>{selected.path || selected.source_ref || selected.id}</Text><Descriptions size="small" column={1} items={[{ key: "links", label: "关系", children: `${selected.in_degree} 入 / ${selected.out_degree} 出` }, { key: "community", label: "社区", children: selected.community || "-" }]} />{selected.path && <Button onClick={() => onOpenFile(selected.path)}>打开证据</Button>}</Space> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="选择节点查看知识上下文" />}</aside></div></Space>;
}
