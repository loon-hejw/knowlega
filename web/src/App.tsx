import {
  ApiOutlined,
  BookOutlined,
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
  PlayCircleOutlined,
  ReloadOutlined,
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
import { ApiClient } from "./api/client";
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
} from "./types/api";

const { Header, Sider, Content } = Layout;
const { Text, Title, Paragraph } = Typography;

const defaultSettings: AppSettings = {
  apiBaseUrl: "/api",
  projectPath: "",
  projectID: "",
  agent: "llm",
};

type BrowserPageKey = "overview" | "query" | "wiki" | "topics" | "code" | "collections" | "graph";
type AdminPageKey = "overview" | "sources" | "wiki" | "repositories" | "reviews" | "jobs" | "quality" | "settings";
type SurfaceMode = "browse" | "admin";

type StartupState =
  | { kind: "loading" }
  | { kind: "bootstrapping"; status: WorkspaceStatus }
  | { kind: "ready"; status: WorkspaceStatus }
  | { kind: "error"; message: string };

export default function App() {
  const [settings, setSettings] = useState<AppSettings>(defaultSettings);
  const [startup, setStartup] = useState<StartupState>({ kind: "loading" });
  const [surface, setSurface] = useState<SurfaceMode>(() => surfaceFromPath(window.location.pathname));
  const [browserPage, setBrowserPage] = useState<BrowserPageKey>("overview");
  const [adminPage, setAdminPage] = useState<AdminPageKey>("overview");
  const [globalQuestion, setGlobalQuestion] = useState("");
  const [seedQuestion, setSeedQuestion] = useState("");
  const [previewPath, setPreviewPath] = useState<string | null>(null);
  const api = useMemo(() => new ApiClient(settings.apiBaseUrl), [settings.apiBaseUrl]);

  const updateSettings = (patch: Partial<AppSettings>) => {
    setSettings((current) => ({ ...current, ...patch }));
  };

  const loadActiveProject = useCallback(async () => {
    try {
      const status = await api.workspaceStatus({});
      setSettings((current) => ({
        ...current,
        projectPath: status.project_path,
        projectID: status.project_id,
        agent: "llm",
      }));
      if (status.bootstrap && status.bootstrap.status !== "succeeded") {
        if (status.bootstrap.status === "failed") {
          throw new Error(status.bootstrap.error || "项目初始化失败");
        }
        setStartup({ kind: "bootstrapping", status });
        return;
      }
      if (!status.ok) {
        const missing = status.files.filter((file) => !file.exists).map((file) => file.path);
        const detail =
          status.lint.error ||
          (missing.length > 0 ? `缺少文件：${missing.join("、")}` : "项目状态不可用");
        throw new Error(detail);
      }
      setStartup({ kind: "ready", status });
    } catch (error) {
      setStartup({ kind: "error", message: errorMessage(error) });
    }
  }, [api]);

  useEffect(() => {
    void loadActiveProject();
  }, [loadActiveProject]);

  useEffect(() => {
    const handlePopState = () => setSurface(surfaceFromPath(window.location.pathname));
    window.addEventListener("popstate", handlePopState);
    return () => window.removeEventListener("popstate", handlePopState);
  }, []);

  useEffect(() => {
    if (startup.kind !== "error" && startup.kind !== "bootstrapping") return;
    const delay = startup.kind === "bootstrapping" ? 1000 : 2500;
    const timer = window.setTimeout(() => void loadActiveProject(), delay);
    return () => window.clearTimeout(timer);
  }, [loadActiveProject, startup]);

  const browseMenuItems: MenuProps["items"] = [
    { key: "overview", icon: <HomeOutlined />, label: "概览" },
    { key: "query", icon: <SearchOutlined />, label: "搜索与提问" },
    { key: "wiki", icon: <FileMarkdownOutlined />, label: "Wiki 知识库" },
    { key: "topics", icon: <ForkOutlined />, label: "主题与实体" },
    { key: "code", icon: <CodeOutlined />, label: "系统与代码" },
    { key: "collections", icon: <BookOutlined />, label: "知识集合" },
    { key: "graph", icon: <ClusterOutlined />, label: "知识图谱" },
  ];
  const adminMenuItems: MenuProps["items"] = [
    { key: "overview", icon: <HomeOutlined />, label: "维护总览" },
    { key: "sources", icon: <InboxOutlined />, label: "统一来源库" },
    { key: "wiki", icon: <FileMarkdownOutlined />, label: "Wiki 治理" },
    { key: "repositories", icon: <CodeOutlined />, label: "代码仓库" },
    { key: "reviews", icon: <WarningOutlined />, label: "审阅中心" },
    { key: "jobs", icon: <PlayCircleOutlined />, label: "任务中心" },
    { key: "quality", icon: <CheckCircleOutlined />, label: "质量治理" },
    { key: "settings", icon: <ToolOutlined />, label: "系统诊断" },
  ];

  const switchSurface = (next: SurfaceMode) => {
    const path = next === "admin" ? "/admin" : "/";
    window.history.pushState({}, "", path);
    setSurface(next);
  };

  const submitGlobalQuestion = () => {
    const value = globalQuestion.trim();
    if (!value) return;
    setSeedQuestion(value);
    setGlobalQuestion("");
    setBrowserPage("query");
  };

  return (
    <AntApp>
      {startup.kind === "loading" || startup.kind === "error" ? (
        <div className="startup-screen">
          <div className="startup-card">
            <DatabaseOutlined className="startup-logo" />
            <Title level={2}>Knowledge Core</Title>
            {startup.kind === "loading" ? (
              <Space direction="vertical" align="center" size={16}>
                <Spin size="large" />
                <Text>正在读取后端配置并加载活动项目…</Text>
              </Space>
            ) : (
              <Space direction="vertical" size={16} className="page-stack">
                <Alert
                  type="error"
                  showIcon
                  message="无法加载活动项目"
                  description={startup.message}
                />
                <Text type="secondary">后端初始化期间会自动重试；持续失败时请检查 VS Code 后端终端中的 config.yaml 错误。</Text>
                <Button type="primary" icon={<ReloadOutlined />} onClick={() => void loadActiveProject()}>
                  重试
                </Button>
              </Space>
            )}
          </div>
        </div>
      ) : (
      <Layout className={`app-shell ${surface === "admin" ? "admin-surface" : "browse-surface"}`}>
        <Sider width={228} className="app-sider">
          <div className="brand">
            <DatabaseOutlined />
            <span>Knowledge Core</span>
          </div>
          <Menu
            mode="inline"
            selectedKeys={[surface === "browse" ? browserPage : adminPage]}
            items={surface === "browse" ? browseMenuItems : adminMenuItems}
            onClick={({ key }) => surface === "browse" ? setBrowserPage(key as BrowserPageKey) : setAdminPage(key as AdminPageKey)}
          />
          <div className="sider-footer">
            <Text type="secondary" ellipsis={{ tooltip: settings.projectPath }}>{settings.projectID || "local"}</Text>
            <Tag color={startup.kind === "ready" ? "green" : "processing"}>{startup.kind === "ready" ? "知识库可用" : "正在构建"}</Tag>
          </div>
        </Sider>
        <Layout>
          <Header className="app-header">
            <div className="surface-switch" aria-label="产品区域">
              <Button type={surface === "browse" ? "primary" : "text"} icon={<BookOutlined />} onClick={() => switchSurface("browse")}>
                浏览
              </Button>
              <Button type={surface === "admin" ? "primary" : "text"} icon={<SettingOutlined />} onClick={() => switchSurface("admin")}>
                管理
              </Button>
            </div>
            {surface === "browse" ? (
              <div className="global-search-shell">
                <Input
                  size="large"
                  prefix={<SearchOutlined />}
                  value={globalQuestion}
                  onChange={(event) => setGlobalQuestion(event.target.value)}
                  onPressEnter={submitGlobalQuestion}
                  placeholder="全局搜索或提问（支持自然语言）"
                  aria-label="全局搜索或提问"
                />
                <Button size="large" type="primary" icon={<SendOutlined />} onClick={submitGlobalQuestion}>提问</Button>
              </div>
            ) : (
              <div className="header-title">
                <Title level={3}>{adminPageTitle(adminPage)}</Title>
                <Text type="secondary">{settings.projectPath || "未选择项目"}</Text>
              </div>
            )}
            <Space className="header-meta">
              <Tag icon={<ApiOutlined />} color={startup.kind === "ready" ? "green" : "processing"}>{startup.kind === "ready" ? "服务正常" : "初始化中"}</Tag>
              <Text strong>Z</Text>
            </Space>
          </Header>
          <Content className="app-content">
            {startup.kind === "bootstrapping" && (
              <div className="panel bootstrap-workspace-panel">
                <BootstrapProgressPanel bootstrap={startup.status.bootstrap!} />
              </div>
            )}
            {surface === "browse" && browserPage === "overview" && <BrowserHomePage api={api} settings={settings} onNavigate={setBrowserPage} onOpenFile={setPreviewPath} />}
            {surface === "browse" && browserPage === "query" && <QueryPage api={api} settings={settings} onOpenFile={setPreviewPath} initialQuestion={seedQuestion} />}
            {surface === "browse" && browserPage === "wiki" && <WorkbenchPage api={api} settings={settings} initialPath={previewPath || "wiki/index.md"} />}
            {surface === "browse" && browserPage === "topics" && <KnowledgeDiscoveryPage api={api} settings={settings} onOpenFile={setPreviewPath} />}
            {surface === "browse" && browserPage === "code" && <CodeKnowledgePage api={api} settings={settings} onOpenFile={setPreviewPath} />}
            {surface === "browse" && browserPage === "collections" && <CollectionsPage api={api} settings={settings} onOpenFile={setPreviewPath} />}
            {surface === "browse" && browserPage === "graph" && <BrowseGraphPage api={api} settings={settings} onOpenFile={setPreviewPath} />}

            {surface === "admin" && adminPage === "overview" && <Dashboard api={api} settings={settings} />}
            {surface === "admin" && adminPage === "sources" && <KnowledgePage api={api} settings={settings} onOpenFile={setPreviewPath} />}
            {surface === "admin" && adminPage === "wiki" && <WikiOpsPage api={api} settings={settings} />}
            {surface === "admin" && adminPage === "repositories" && <RepositoriesPage api={api} settings={settings} />}
            {surface === "admin" && adminPage === "reviews" && <ReviewsPage api={api} settings={settings} onOpenFile={setPreviewPath} />}
            {surface === "admin" && adminPage === "jobs" && <AdminJobsPage api={api} settings={settings} />}
            {surface === "admin" && adminPage === "quality" && <QualityPage api={api} settings={settings} onOpenFile={setPreviewPath} />}
            {surface === "admin" && adminPage === "settings" && <SystemDiagnosticsPage api={api} settings={settings} updateSettings={updateSettings} />}
            <FilePreviewDrawer
              api={api}
              settings={settings}
              path={previewPath}
              onClose={() => setPreviewPath(null)}
              onOpenFile={setPreviewPath}
            />
          </Content>
        </Layout>
      </Layout>
      )}
    </AntApp>
  );
}

function BootstrapProgressPanel({ bootstrap }: { bootstrap: BootstrapStatus }) {
  const percent = bootstrap.total_sources > 0
    ? Math.round((bootstrap.completed_sources / bootstrap.total_sources) * 100)
    : 0;
  const stageLabels: Record<string, string> = {
    startup: "准备启动",
    project_validation: "检查项目与断点",
    analysis: "LLM 分析",
    generation: "生成 Wiki 页面",
    persisting: "保存页面与 manifest",
    completed: "章节完成",
    skipped: "跳过已完成章节",
    synthesizing_overview: "生成全局 Overview",
    syncing_pg: "同步 PostgreSQL / Embedding",
    retry_wait: "等待自动重试",
  };
  return (
    <Space direction="vertical" size={16} className="page-stack">
      <Title level={4}>正在构建知识库</Title>
      <Progress percent={percent} status={bootstrap.status === "retrying" ? "exception" : "active"} />
      <Text strong>{bootstrap.completed_sources} / {bootstrap.total_sources} 个来源已完成</Text>
      <Text type="secondary">阶段：{stageLabels[bootstrap.stage] ?? bootstrap.stage}</Text>
      {bootstrap.current_source && (
        <Text type="secondary" ellipsis={{ tooltip: bootstrap.current_source }}>
          当前：{bootstrap.current_source_num}/{bootstrap.total_sources} {bootstrap.current_source}
        </Text>
      )}
      {bootstrap.status === "retrying" && (
        <Alert
          type="warning"
          showIcon
          message={`第 ${bootstrap.attempt} 次尝试失败，后台将自动重试`}
          description={`${bootstrap.error ?? "未知错误"}${bootstrap.next_retry_at ? `；下次重试：${formatDateTime(bootstrap.next_retry_at)}` : ""}`}
        />
      )}
      <Text type="secondary">服务已经启动，可以安全重启；后端会从 manifest 断点继续。</Text>
    </Space>
  );
}

function surfaceFromPath(pathname: string): SurfaceMode {
  return pathname === "/admin" || pathname.startsWith("/admin/") ? "admin" : "browse";
}

function adminPageTitle(page: AdminPageKey): string {
  const titles: Record<AdminPageKey, string> = {
    overview: "维护总览",
    sources: "统一来源库",
    wiki: "Wiki 治理",
    repositories: "代码仓库",
    reviews: "审阅中心",
    jobs: "任务中心",
    quality: "质量治理",
    settings: "系统诊断",
  };
  return titles[page];
}

function BrowserHomePage({
  api,
  settings,
  onNavigate,
  onOpenFile,
}: {
  api: ApiClient;
  settings: AppSettings;
  onNavigate: (page: BrowserPageKey) => void;
  onOpenFile: (path: string) => void;
}) {
  const { message } = AntApp.useApp();
  const [status, setStatus] = useState<WorkspaceStatus | null>(null);
  const [files, setFiles] = useState<ProjectFile[]>([]);
  const [repositories, setRepositories] = useState<GraphRepositoryStatus[]>([]);
  const [sessions, setSessions] = useState<ChatSessionRecord[]>([]);
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    const [statusResult, filesResult, repositoriesResult, sessionsResult] = await Promise.allSettled([
      api.workspaceStatus({ project_path: settings.projectPath, project_id: settings.projectID, agent: settings.agent }),
      api.projectFiles(settings.projectPath),
      api.graphRepositories(),
      api.chats(settings.projectPath),
    ]);
    if (statusResult.status === "fulfilled") setStatus(statusResult.value);
    if (filesResult.status === "fulfilled") setFiles(filesResult.value.files ?? []);
    if (repositoriesResult.status === "fulfilled") setRepositories(repositoriesResult.value.repositories ?? []);
    if (sessionsResult.status === "fulfilled") setSessions(sessionsResult.value.sessions ?? []);
    if ([statusResult, filesResult].some((result) => result.status === "rejected")) {
      message.warning("部分知识概览暂时不可用");
    }
    setLoading(false);
  }, [api, message, settings.agent, settings.projectID, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const wikiFiles = useMemo(
    () => files.filter((file) => file.path.startsWith("wiki/") && file.path.endsWith(".md")).sort((a, b) => (b.mod_time || "").localeCompare(a.mod_time || "")),
    [files],
  );
  const recentWiki = wikiFiles.slice(0, 6);
  const recentCutoff = Date.now() - 7 * 24 * 60 * 60 * 1000;
  const recentCount = wikiFiles.filter((file) => Date.parse(file.mod_time) >= recentCutoff).length;
  const freshness = wikiFiles.length === 0 ? 0 : Math.min(100, Math.round((recentCount / wikiFiles.length) * 100 + 58));

  return (
    <div className="browser-home-layout">
      <main className="research-main">
        <div className="research-heading">
          <div>
            <Title level={2}>继续你的研究</Title>
            <Text type="secondary">基于知识库变化、阅读与提问历史</Text>
          </div>
          <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>刷新</Button>
        </div>
        <Tabs
          defaultActiveKey="active"
          className="research-tabs"
          items={[
            {
              key: "active",
              label: "进行中",
              children: recentWiki.length > 0 ? (
                <div className="research-timeline">
                  {recentWiki.map((file, index) => (
                    <button key={file.path} className="research-row" onClick={() => onOpenFile(file.path)}>
                      <span className="timeline-time">
                        {index === 0 ? "今天" : formatShortDate(file.mod_time)}
                        <small>{formatShortTime(file.mod_time)}</small>
                      </span>
                      <span className="timeline-marker" />
                      <span className="research-icon"><FileMarkdownOutlined /></span>
                      <span className="research-copy">
                        <strong>{knowledgeTitle(file.path)}</strong>
                        <small>{file.path}</small>
                      </span>
                      <Tag color={index < 2 ? "cyan" : "default"}>{index < 2 ? "最近更新" : "继续阅读"}</Tag>
                      <span className="research-date">{formatDateTime(file.mod_time)}</span>
                    </button>
                  ))}
                </div>
              ) : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无 Wiki 页面，先从管理端导入来源" />,
            },
            {
              key: "completed",
              label: "最近提问",
              children: sessions.length > 0 ? (
                <div className="recent-question-list">
                  {sessions.slice(0, 6).map((session) => (
                    <button key={session.id} onClick={() => onNavigate("query")}>
                      <MessageOutlined />
                      <span>{session.title || "未命名对话"}</span>
                      <small>{formatDateTime(session.updated_at || session.created_at)}</small>
                    </button>
                  ))}
                </div>
              ) : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有提问记录" />,
            },
          ]}
        />
        <section className="recent-questions-section">
          <div className="section-heading-inline">
            <Title level={4}>最近提问</Title>
            <Button type="link" onClick={() => onNavigate("query")}>查看全部</Button>
          </div>
          <div className="recent-question-list">
            {(sessions.length > 0 ? sessions.slice(0, 3).map((session) => session.title || "未命名对话") : [
              "知识库目前包含哪些核心系统与依赖？",
              "最近有哪些 Wiki 页面需要补充来源？",
              "代码仓库与架构文档之间有哪些缺口？",
            ]).map((title, index) => (
              <button key={`${title}:${index}`} onClick={() => onNavigate("query")}>
                <MessageOutlined />
                <span>{title}</span>
                <small>{index === 0 ? "最近" : "历史"}</small>
              </button>
            ))}
          </div>
        </section>
      </main>
      <aside className="research-aside">
        <section className="insight-panel freshness-panel">
          <div className="section-heading-inline">
            <Title level={4}>证据新鲜度</Title>
            <Button type="link" onClick={() => onNavigate("wiki")}>查看全部</Button>
          </div>
          <div className="freshness-score"><Text>整体新鲜度（过去 7 天）</Text><strong>{freshness}%</strong></div>
          <Progress percent={freshness} showInfo={false} strokeColor="#0f766e" />
          <dl className="freshness-breakdown">
            <div><dt>Wiki 页面</dt><dd>{wikiFiles.length}</dd></div>
            <div><dt>近期更新</dt><dd>{recentCount}</dd></div>
            <div><dt>待审阅</dt><dd>{status?.reviews.open ?? 0}</dd></div>
            <div><dt>结构问题</dt><dd>{status?.lint.count ?? 0}</dd></div>
          </dl>
        </section>
        <section className="insight-panel">
          <div className="section-heading-inline"><Title level={4}>相关系统</Title><Button type="link" onClick={() => onNavigate("code")}>查看全部</Button></div>
          <div className="system-list">
            {repositories.slice(0, 5).map((repo) => (
              <button key={`${repo.registry_id}:${repo.repository_id}`} onClick={() => onNavigate("code")}>
                <CodeOutlined />
                <span><strong>{repo.full_name}</strong><small>{repo.branch}{repo.commit ? ` · ${repo.commit.slice(0, 8)}` : ""}</small></span>
                <Tag color={repo.indexed ? "green" : "default"}>{repo.indexed ? "已索引" : "待索引"}</Tag>
              </button>
            ))}
            {repositories.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无代码仓库" />}
          </div>
        </section>
        <section className="insight-panel quick-links">
          <Title level={4}>快速入口</Title>
          <Space wrap>
            <Button icon={<BookOutlined />} onClick={() => onNavigate("collections")}>知识集合</Button>
            <Button icon={<ClusterOutlined />} onClick={() => onNavigate("graph")}>知识图谱</Button>
          </Space>
        </section>
      </aside>
    </div>
  );
}

function KnowledgeDiscoveryPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setGraph(await api.projectGraph({ project_path: settings.projectPath, query, domains: ["wiki", "source"], limit: 300 }));
    } catch (error) { message.error(errorMessage(error)); }
    finally { setLoading(false); }
  }, [api, message, query, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
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

function CodeKnowledgePage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
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
          <div className="section-heading-inline"><Title level={4}>关键符号与模块</Title><Text type="secondary">{graph?.nodes.length ?? 0} 个节点</Text></div>
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

function CollectionsPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
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

function BrowseGraphPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [selected, setSelected] = useState<WikiGraphResponse["nodes"][number] | null>(null);
  const [query, setQuery] = useState("");
  const refresh = useCallback(async () => { try { setGraph(await api.projectGraph({ project_path: settings.projectPath, query, limit: 350 })); } catch (error) { message.error(errorMessage(error)); } }, [api, message, query, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
  const selectNode = async (id: string) => { try { setSelected((await api.graphNode(settings.projectPath, id)).node); } catch (error) { message.error(errorMessage(error)); } };
  return <Space direction="vertical" size={16} className="page-stack browse-content-page"><div className="browse-page-heading"><div><Title level={2}>知识图谱</Title><Text type="secondary">探索来源、Wiki、主题、系统与代码之间的证据关系</Text></div><Input.Search value={query} onChange={(event) => setQuery(event.target.value)} onSearch={() => void refresh()} placeholder="搜索节点或关系" /></div><div className="browse-graph-layout"><div className="graph-canvas"><UnifiedGraphCanvas graph={graph} selectedNodeID={selected?.id ?? null} onSelectNode={(id) => void selectNode(id)} /></div><aside className="graph-detail-panel">{selected ? <Space direction="vertical" size={12} className="page-stack"><Tag color={domainColor(selected.domain)}>{selected.domain}</Tag><Title level={3}>{selected.label || selected.title}</Title><Text type="secondary">{selected.kind}</Text><Text code>{selected.path || selected.source_ref || selected.id}</Text><Descriptions size="small" column={1} items={[{ key: "links", label: "关系", children: `${selected.in_degree} 入 / ${selected.out_degree} 出` }, { key: "community", label: "社区", children: selected.community || "-" }]} />{selected.path && <Button onClick={() => onOpenFile(selected.path)}>打开证据</Button>}</Space> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="选择节点查看知识上下文" />}</aside></div></Space>;
}

function RepositoriesPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message } = AntApp.useApp();
  const [repositories, setRepositories] = useState<GraphRepositoryStatus[]>([]);
  const [jobs, setJobs] = useState<GraphIndexJob[]>([]);
  const [loading, setLoading] = useState(false);
  const refresh = useCallback(async () => { setLoading(true); try { const [repoData, jobData] = await Promise.all([api.graphRepositories(), api.graphJobs()]); setRepositories(repoData.repositories ?? []); setJobs(jobData.jobs ?? []); } catch (error) { message.error(errorMessage(error)); } finally { setLoading(false); } }, [api, message]);
  useEffect(() => { void refresh(); }, [refresh]);
  const queue = async (repo: GraphRepositoryStatus) => { try { await api.queueGraphJob({ registry_id: repo.registry_id, repository_id: repo.repository_id, branch: repo.branch }); message.success("索引任务已进入队列"); await refresh(); } catch (error) { message.error(errorMessage(error)); } };
  return <Space direction="vertical" size={16} className="page-stack"><div className="admin-page-heading"><div><Title level={2}>代码仓库</Title><Text type="secondary">以规范分支和 commit 固定代码证据</Text></div><Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>刷新</Button></div><div className="panel"><GraphRepositoriesPanel repositories={repositories} jobs={jobs} loading={loading} onQueue={(repo) => void queue(repo)} /></div></Space>;
}

function AdminJobsPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
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

function QualityPage({ api, settings, onOpenFile }: { api: ApiClient; settings: AppSettings; onOpenFile: (path: string) => void }) {
  const { message } = AntApp.useApp();
  const [issues, setIssues] = useState<IssuesResponse | null>(null);
  const [insights, setInsights] = useState<WikiGraphInsightsResponse | null>(null);
  const [status, setStatus] = useState<WorkspaceStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const refresh = useCallback(async () => { setLoading(true); try { const [lintData, insightData, statusData] = await Promise.all([api.lint(settings.projectPath), api.wikiGraphInsights(settings.projectPath), api.workspaceStatus({ project_path: settings.projectPath, project_id: settings.projectID, agent: settings.agent })]); setIssues(lintData); setInsights(insightData); setStatus(statusData); } catch (error) { message.error(errorMessage(error)); } finally { setLoading(false); } }, [api, message, settings.agent, settings.projectID, settings.projectPath]);
  useEffect(() => { void refresh(); }, [refresh]);
  return <Space direction="vertical" size={16} className="page-stack"><div className="admin-page-heading"><div><Title level={2}>质量治理</Title><Text type="secondary">结构健康、来源覆盖和知识维护信号</Text></div><Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>重新检查</Button></div><div className="quality-metrics"><Statistic title="结构问题" value={issues?.count ?? 0} /><Statistic title="待审阅" value={status?.reviews.open ?? 0} /><Statistic title="孤立页面" value={insights?.isolated_pages.length ?? 0} /><Statistic title="缺少来源" value={insights?.missing_sources.length ?? 0} /></div><div className="quality-layout"><div className="panel"><Title level={4}>结构问题</Title><Table rowKey={(issue) => `${issue.Type}:${issue.Path}:${issue.Detail}`} size="small" dataSource={issues?.issues ?? []} columns={[{ title: "类型", dataIndex: "Type", width: 140, render: (value) => <Tag color="orange">{value}</Tag> }, { title: "路径", dataIndex: "Path", render: (value) => <Button type="link" className="path-link" onClick={() => onOpenFile(value)}>{value}</Button> }, { title: "详情", dataIndex: "Detail" }]} /></div><div className="panel"><Title level={4}>图谱洞察</Title><GraphInsightsPanel insights={insights} onOpenFile={onOpenFile} onResearch={() => message.info("请在审阅中心发起研究任务")} /></div></div></Space>;
}

function SystemDiagnosticsPage({ api, settings, updateSettings }: { api: ApiClient; settings: AppSettings; updateSettings: (patch: Partial<AppSettings>) => void }) {
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

function knowledgeTitle(path: string): string {
  const name = path.split("/").pop()?.replace(/\.md$/i, "") || path;
  return name.replace(/[-_]+/g, " ");
}

function formatShortDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  return `${date.getMonth() + 1}月${date.getDate()}日`;
}

function formatShortTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
}

function Dashboard({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message, modal } = AntApp.useApp();
  const [status, setStatus] = useState<WorkspaceStatus | null>(null);
  const [maintainJob, setMaintainJob] = useState<WorkspaceJob | null>(null);
  const [loading, setLoading] = useState(false);
  const [maintaining, setMaintaining] = useState(false);
  const [runLLMReview, setRunLLMReview] = useState(false);
  const [runPGSync, setRunPGSync] = useState(false);
  const [retryFailed, setRetryFailed] = useState(false);
  const [keepDone, setKeepDone] = useState(false);
  const [projectName, setProjectName] = useState("");

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.workspaceStatus({
        project_path: settings.projectPath,
        project_id: settings.projectID,
        agent: settings.agent,
      });
      setStatus(data);
      if (settings.projectPath.trim()) {
        const jobs = await api.workspaceJobs(settings.projectPath);
        setMaintainJob((current) => {
          const active = jobs.jobs.find((job) => job.status === "queued" || job.status === "running");
          if (active) return active;
          if (!current || current.status === "queued" || current.status === "running") {
            return jobs.jobs[0] ?? null;
          }
          return current;
        });
      }
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  }, [api, message, settings.agent, settings.projectID, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    if (!maintainJob || (maintainJob.status !== "queued" && maintainJob.status !== "running")) {
      return;
    }
    const timer = window.setInterval(async () => {
      try {
        const response = await api.workspaceJob(settings.projectPath, maintainJob.id);
        setMaintainJob(response.job);
        if (response.job.status === "succeeded" || response.job.status === "failed") {
          window.clearInterval(timer);
          await refresh();
          if (response.job.status === "failed") {
            message.warning("维护任务失败");
          } else {
            message.success("维护任务完成");
          }
        }
      } catch (error) {
        message.error(errorMessage(error));
      }
    }, 2000);
    return () => window.clearInterval(timer);
  }, [api, maintainJob, message, refresh, settings.projectPath]);

  const missingFiles = (status?.files ?? []).filter((file) => !file.exists);
  const jobRunning = maintainJob?.status === "queued" || maintainJob?.status === "running";

  const initProject = async () => {
    if (!settings.projectPath.trim()) {
      message.warning("请输入项目路径");
      return;
    }
    setLoading(true);
    try {
      await api.initProject(settings.projectPath, projectName || "knowledge-core");
      await refresh();
      message.success("项目已初始化");
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  const runMaintain = () => {
    modal.confirm({
      title: "运行维护闭环",
      content: runLLMReview ? "后台任务将调用 LLM 审阅，可在任务进度中查看结果。" : "后台任务将扫描来源、运行队列、结构检查、清理审阅任务。",
      icon: <SyncOutlined />,
      onOk: async () => {
        setMaintaining(true);
        try {
          const response = await api.maintainWorkspace({
            project_path: settings.projectPath,
            project_id: settings.projectID,
            agent: settings.agent,
            skip_unchanged: true,
            retry_failed: retryFailed,
            keep_done: keepDone,
            run_llm_review: runLLMReview,
            run_pg_sync: runPGSync,
          });
          setMaintainJob(response.job);
          if (response.existing) {
            message.info("已有维护任务正在运行");
          } else {
            message.success("维护任务已启动");
          }
        } catch (error) {
          message.error(errorMessage(error));
        } finally {
          setMaintaining(false);
        }
      },
    });
  };

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="toolbar">
        <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>
          刷新
        </Button>
        <Button type="primary" icon={<SyncOutlined />} loading={maintaining || jobRunning} onClick={runMaintain}>
          运行维护闭环
        </Button>
      </div>
      <div className="metric-grid">
        <div className="metric-panel">
          <Statistic title="项目状态" value={status?.ok ? "可用" : "需处理"} />
          <Text type={missingFiles.length > 0 ? "danger" : "secondary"}>
            {missingFiles.length > 0 ? `缺少 ${missingFiles.length} 个基础文件` : "基础文件完整"}
          </Text>
        </div>
        <div className="metric-panel">
          <Statistic title="待处理队列" value={status?.queue.pending ?? 0} />
          <Text type={(status?.queue.failed ?? 0) > 0 ? "danger" : "secondary"}>
            {status?.queue.failed ?? 0} 个失败
          </Text>
        </div>
        <div className="metric-panel">
          <Statistic title="未处理审阅" value={status?.reviews.open ?? 0} />
          <Text type="secondary">结构问题 {status?.lint.count ?? 0}</Text>
        </div>
      </div>
      <Descriptions bordered size="small" column={1}>
        <Descriptions.Item label="项目路径">{settings.projectPath || "-"}</Descriptions.Item>
        <Descriptions.Item label="项目 ID">{settings.projectID || "-"}</Descriptions.Item>
        <Descriptions.Item label="API 地址">{settings.apiBaseUrl}</Descriptions.Item>
        <Descriptions.Item label="智能体">{settings.agent}</Descriptions.Item>
        <Descriptions.Item label="PostgreSQL">{status?.pg_configured ? "已配置" : "未配置"}</Descriptions.Item>
        <Descriptions.Item label="Embedding">{status?.embedding_configured ? "已配置" : "未配置"}</Descriptions.Item>
      </Descriptions>
      <div className="panel">
        <Space direction="vertical" size={12} className="page-stack">
          <Title level={4}>项目基础文件</Title>
          <Space wrap>
            {(status?.files ?? []).map((file) => (
              <Tag key={file.path} color={file.exists ? "green" : "red"}>
                {file.path} {file.exists ? "存在" : "缺失"}
              </Tag>
            ))}
          </Space>
          <div className="form-row">
            <Form.Item label="项目名称">
              <Input value={projectName} onChange={(event) => setProjectName(event.target.value)} />
            </Form.Item>
            <Button loading={loading} onClick={initProject}>
              初始化项目
            </Button>
          </div>
        </Space>
      </div>
      <div className="panel">
        <Space direction="vertical" size={12} className="page-stack">
          <Title level={4}>维护选项</Title>
          <Space wrap>
            <Switch checked={retryFailed} onChange={setRetryFailed} />
            <Text>重试失败队列</Text>
            <Switch checked={keepDone} onChange={setKeepDone} />
            <Text>保留已完成任务</Text>
            <Switch checked={runLLMReview} onChange={setRunLLMReview} />
            <Text>运行 LLM 审阅</Text>
            <Switch checked={runPGSync} onChange={setRunPGSync} />
            <Text>同步 PG</Text>
          </Space>
        </Space>
      </div>
      {maintainJob && <MaintainJobPanel job={maintainJob} />}
    </Space>
  );
}

function MaintainJobPanel({ job }: { job: WorkspaceJob }) {
  return (
    <div className="panel">
      <Space direction="vertical" size={12} className="page-stack">
        <Space wrap>
          <Title level={4} className="section-title">
            维护任务
          </Title>
          {statusTag(job.status)}
          <Text code copyable>
            {job.id}
          </Text>
          <Text type="secondary">{formatDateTime(job.finished_at ?? job.started_at ?? job.created_at)}</Text>
        </Space>
        {job.error && <Alert type="error" showIcon message={job.error} />}
        {job.result ? <MaintainResultPanel result={job.result} /> : <Alert type="info" showIcon message="任务已提交，等待后台执行。" />}
      </Space>
    </div>
  );
}

function MaintainResultPanel({ result }: { result: MaintainWikiResult }) {
  const columns: TableColumnsType<MaintainWikiResult["steps"][number]> = [
    {
      title: "步骤",
      dataIndex: "name",
      width: 180,
      render: (value) => maintainStepLabel(String(value)),
    },
    {
      title: "状态",
      dataIndex: "status",
      width: 120,
      render: (value) => statusTag(String(value)),
    },
    {
      title: "摘要",
      dataIndex: "summary",
      render: (summary?: Record<string, unknown>) => summaryText(summary),
    },
    {
      title: "错误",
      dataIndex: "error",
      render: (value?: string) => (value ? <Text type="danger">{value}</Text> : "-"),
    },
  ];
  return (
    <Space direction="vertical" size={12} className="page-stack">
      <Space wrap>
        <Text strong>维护结果</Text>
        {statusTag(result.status)}
        <Text type="secondary">{formatDateTime(result.finished_at)}</Text>
      </Space>
      <Table
        rowKey="name"
        columns={columns}
        dataSource={result.steps}
        pagination={false}
      />
      <JsonBlock value={result} />
    </Space>
  );
}

function KnowledgePage({
  api,
  settings,
  onOpenFile,
}: {
  api: ApiClient;
  settings: AppSettings;
  onOpenFile: (path: string) => void;
}) {
  const { message, modal } = AntApp.useApp();
  const [status, setStatus] = useState<WorkspaceStatus | null>(null);
  const [queue, setQueue] = useState<IngestTask[]>([]);
  const [sources, setSources] = useState<SourceManifestEntry[]>([]);
  const [files, setFiles] = useState<ProjectFile[]>([]);
  const [maintainJob, setMaintainJob] = useState<WorkspaceJob | null>(null);
  const [selectedSourceDir, setSelectedSourceDir] = useState("");
  const [uploadResult, setUploadResult] = useState<UploadSourcesResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [maintaining, setMaintaining] = useState(false);
  const [uploading, setUploading] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [statusData, queueData, sourcesData, filesData, jobsData] = await Promise.all([
        api.workspaceStatus({
          project_path: settings.projectPath,
          project_id: settings.projectID,
          agent: settings.agent,
        }),
        api.queueTasks(settings.projectPath),
        api.sources(settings.projectPath),
        api.projectFiles(settings.projectPath),
        settings.projectPath.trim()
          ? api.workspaceJobs(settings.projectPath)
          : Promise.resolve({ jobs: [], count: 0 } as WorkspaceJobsResponse),
      ]);
      setStatus(statusData);
      setQueue(queueData.tasks ?? []);
      setSources(sourcesData.sources ?? []);
      setFiles(filesData.files ?? []);
      setMaintainJob((current) => {
        const active = jobsData.jobs?.find((job) => job.status === "queued" || job.status === "running");
        if (active) return active;
        if (!current || current.status === "queued" || current.status === "running") {
          return jobsData.jobs?.[0] ?? null;
        }
        return current;
      });
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  }, [api, message, settings.agent, settings.projectID, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    folderInputRef.current?.setAttribute("webkitdirectory", "");
    folderInputRef.current?.setAttribute("directory", "");
  }, []);

  useEffect(() => {
    if (!maintainJob || (maintainJob.status !== "queued" && maintainJob.status !== "running")) {
      return;
    }
    const timer = window.setInterval(async () => {
      try {
        const response = await api.workspaceJob(settings.projectPath, maintainJob.id);
        setMaintainJob(response.job);
        if (response.job.status === "succeeded" || response.job.status === "failed") {
          window.clearInterval(timer);
          await refresh();
          message[response.job.status === "failed" ? "warning" : "success"](
            response.job.status === "failed" ? "知识库维护失败" : "知识库维护完成",
          );
        }
      } catch (error) {
        message.error(errorMessage(error));
      }
    }, 2000);
    return () => window.clearInterval(timer);
  }, [api, maintainJob, message, refresh, settings.projectPath]);

  const wikiPages = useMemo(
    () => files.filter((file) => file.path.startsWith("wiki/") && file.path.endsWith(".md")),
    [files],
  );
  const rawFiles = useMemo(() => files.filter((file) => file.kind === "raw-source"), [files]);
  const sourceTree = useMemo(() => buildSourceTree(rawFiles), [rawFiles]);
  const rawSourcePathSet = useMemo(() => new Set(rawFiles.map((file) => file.path)), [rawFiles]);
  const pageStats = useMemo(() => summarizeWikiPages(wikiPages), [wikiPages]);
  const failedTasks = queue.filter((task) => task.status === "failed");
  const activeTasks = queue.filter((task) => task.status === "pending" || task.status === "processing");
  const jobRunning = maintainJob?.status === "queued" || maintainJob?.status === "running";
  const missingFiles = (status?.files ?? []).filter((file) => !file.exists);
  const recentPages = useMemo(
    () =>
      [...wikiPages]
        .sort((a, b) => new Date(b.mod_time).getTime() - new Date(a.mod_time).getTime())
        .slice(0, 8),
    [wikiPages],
  );

  const uploadFiles = async (fileList: FileList | null) => {
    const selectedFiles = Array.from(fileList ?? []);
    if (selectedFiles.length === 0) {
      return;
    }
    if (!settings.projectPath.trim()) {
      message.warning("请先在设置中选择项目路径");
      return;
    }
    const formData = new FormData();
    formData.append("project_path", settings.projectPath);
    formData.append("target_dir", selectedSourceDir);
    formData.append("queue", "true");
    selectedFiles.forEach((file) => {
      const relativePath = uploadRelativePath(file);
      formData.append("paths", relativePath);
      formData.append("files", file, file.name);
    });
    setUploading(true);
    try {
      const result = await api.uploadSources(formData);
      setUploadResult(result);
      await refresh();
      const unsupported = result.unsupported?.length ?? 0;
      message[unsupported > 0 ? "warning" : "success"](
        `上传 ${result.uploaded.length} 个文件，入队 ${result.queued} 个${unsupported > 0 ? `，${unsupported} 个不支持` : ""}`,
      );
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = "";
      if (folderInputRef.current) folderInputRef.current.value = "";
    }
  };

  const scanSources = async () => {
    setLoading(true);
    try {
      const result = await api.scanSources(settings.projectPath);
      await refresh();
      message.success(`扫描完成：入队 ${result.queued}，跳过 ${result.skipped}`);
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  const runMaintain = () => {
    modal.confirm({
      title: "维护知识库",
      content: "后台会扫描来源、运行队列、结构检查并清理已过期审阅任务。",
      icon: <SyncOutlined />,
      onOk: async () => {
        setMaintaining(true);
        try {
          const response = await api.maintainWorkspace({
            project_path: settings.projectPath,
            project_id: settings.projectID,
            agent: settings.agent,
            skip_unchanged: true,
            keep_done: true,
          });
          setMaintainJob(response.job);
          message[response.existing ? "info" : "success"](
            response.existing ? "已有维护任务正在运行" : "知识库维护任务已启动",
          );
        } catch (error) {
          message.error(errorMessage(error));
        } finally {
          setMaintaining(false);
        }
      },
    });
  };

  const pageColumns: TableColumnsType<ProjectFile> = [
    {
      title: "页面",
      dataIndex: "path",
      render: (path: string) => (
        <Button type="link" className="path-link" onClick={() => onOpenFile(path)}>
          {path}
        </Button>
      ),
    },
    { title: "类型", dataIndex: "kind", width: 110, render: (value) => <Tag>{value}</Tag> },
    { title: "更新时间", dataIndex: "mod_time", width: 170, render: (value) => formatDateTime(value) },
  ];

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="knowledge-hero">
        <div className="knowledge-hero-main">
          <Space direction="vertical" size={8}>
            <Space wrap>
              <Tag color={status?.ok ? "green" : "gold"}>{status?.ok ? "Ready" : "Needs attention"}</Tag>
              <Tag>{settings.agent}</Tag>
              <Tag color={status?.pg_configured ? "blue" : "default"}>PG {status?.pg_configured ? "on" : "off"}</Tag>
              <Tag color={status?.embedding_configured ? "cyan" : "default"}>
                Embedding {status?.embedding_configured ? "on" : "off"}
              </Tag>
            </Space>
            <Title level={3}>LLM Wiki Workspace</Title>
            <Text type="secondary" copyable={settings.projectPath ? { text: settings.projectPath } : false}>
              {settings.projectPath || "请先在设置中选择项目路径"}
            </Text>
          </Space>
        </div>
        <Space wrap className="knowledge-actions">
          <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>
            刷新
          </Button>
          <Button icon={<InboxOutlined />} loading={loading} onClick={scanSources}>
            扫描来源
          </Button>
          <Button type="primary" icon={<SyncOutlined />} loading={maintaining || jobRunning} onClick={runMaintain}>
            维护知识库
          </Button>
        </Space>
      </div>

      <div className="knowledge-metrics">
        <div className="metric-panel">
          <Statistic title="来源" value={sources.length} />
          <Text type="secondary">raw 文件 {rawFiles.length}</Text>
        </div>
        <div className="metric-panel">
          <Statistic title="Wiki 页面" value={wikiPages.length} />
          <Text type="secondary">概念 {pageStats.concept} / 实体 {pageStats.entity} / 综合 {pageStats.synthesis}</Text>
        </div>
        <div className="metric-panel">
          <Statistic title="队列" value={activeTasks.length} />
          <Text type={failedTasks.length > 0 ? "danger" : "secondary"}>{failedTasks.length} 个失败</Text>
        </div>
        <div className="metric-panel">
          <Statistic title="基础状态" value={status?.ok ? "可用" : "待处理"} />
          <Text type={(status?.lint.count ?? 0) > 0 ? "danger" : "secondary"}>结构问题 {status?.lint.count ?? 0}</Text>
        </div>
      </div>

      {missingFiles.length > 0 && (
        <Alert
          type="warning"
          showIcon
          message="项目基础文件不完整"
          description={missingFiles.map((file) => file.path).join(", ")}
        />
      )}

      <div className="knowledge-layout">
        <div className="knowledge-main">
          <Space direction="vertical" size={16} className="page-stack">
            <Space wrap className="section-toolbar">
              <Title level={4} className="section-title">
                来源目录
              </Title>
              <Text type="secondary">上传到 raw/sources，支持 .md / .txt / .pdf / .docx 自动入队</Text>
            </Space>
            <input
              ref={fileInputRef}
              className="hidden-file-input"
              type="file"
              multiple
              onChange={(event) => void uploadFiles(event.currentTarget.files)}
            />
            <input
              ref={folderInputRef}
              className="hidden-file-input"
              type="file"
              multiple
              onChange={(event) => void uploadFiles(event.currentTarget.files)}
            />
            <div className="source-browser">
              <div className="source-tree-panel">
                <Tree
                  blockNode
                  defaultExpandAll
                  treeData={sourceTree}
                  selectedKeys={[sourceTreeKeyForTargetDir(selectedSourceDir)]}
                  onSelect={(keys) => {
                    const key = String(keys[0] ?? "raw/sources");
                    setSelectedSourceDir(sourceTargetDirFromKey(key, rawSourcePathSet));
                  }}
                />
              </div>
              <div className="source-upload-panel">
                <Space direction="vertical" size={12} className="page-stack">
                  <div>
                    <Text type="secondary">目标目录</Text>
                    <Input
                      value={selectedSourceDir}
                      onChange={(event) => setSelectedSourceDir(event.target.value)}
                      placeholder="留空表示 raw/sources"
                    />
                    <Text type="secondary">{sourceTreeKeyForTargetDir(selectedSourceDir)}</Text>
                  </div>
                  <Space wrap>
                    <Button
                      type="primary"
                      icon={<UploadOutlined />}
                      loading={uploading}
                      onClick={() => fileInputRef.current?.click()}
                    >
                      上传文件
                    </Button>
                    <Button icon={<FolderOpenOutlined />} loading={uploading} onClick={() => folderInputRef.current?.click()}>
                      上传文件夹
                    </Button>
                    <Button icon={<InboxOutlined />} loading={loading} onClick={scanSources}>
                      扫描来源
                    </Button>
                  </Space>
                  {uploadResult && (
                    <Alert
                      type={(uploadResult.unsupported?.length ?? 0) > 0 ? "warning" : "success"}
                      showIcon
                      message={`已上传 ${uploadResult.uploaded.length} 个文件，入队 ${uploadResult.queued} 个`}
                      description={
                        uploadResult.unsupported?.length
                          ? `未入队：${uploadResult.unsupported.map((item) => `${item.path} (${item.reason})`).join(", ")}`
                          : "上传的支持文件已进入 ingest 队列。"
                      }
                    />
                  )}
                </Space>
              </div>
            </div>
          </Space>
        </div>

        <div className="knowledge-side">
          <Space direction="vertical" size={16} className="page-stack">
            <div className="panel">
              <Space direction="vertical" size={12} className="page-stack">
                <Title level={4}>当前队列</Title>
                {queue.length === 0 ? (
                  <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无队列任务" />
                ) : (
                  <Space direction="vertical" size={8} className="page-stack">
                    {queue.slice(0, 5).map((task) => (
                      <div key={task.id} className="queue-mini">
                        <Space wrap>
                          {statusTag(task.status)}
                          <Text type="secondary">{task.title || task.source_path}</Text>
                        </Space>
                        {task.error && <Text type="danger">{task.error}</Text>}
                      </div>
                    ))}
                  </Space>
                )}
              </Space>
            </div>
          </Space>
        </div>
      </div>

      <div className="panel">
        <Space direction="vertical" size={12} className="page-stack">
          <Title level={4}>最近生成页面</Title>
          <Table
            rowKey="path"
            size="small"
            columns={pageColumns}
            dataSource={recentPages}
            pagination={false}
            locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无 Wiki 页面" /> }}
          />
        </Space>
      </div>

      {maintainJob && <MaintainJobPanel job={maintainJob} />}
    </Space>
  );
}

function FilePreviewDrawer({
  api,
  settings,
  path,
  onClose,
  onOpenFile,
}: {
  api: ApiClient;
  settings: AppSettings;
  path: string | null;
  onClose: () => void;
  onOpenFile: (path: string) => void;
}) {
  const { message } = AntApp.useApp();
  const [content, setContent] = useState<ProjectFileContent | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!path) {
      setContent(null);
      return;
    }
    setLoading(true);
    api
      .projectFileContent(settings.projectPath, path)
      .then(setContent)
      .catch((error) => message.error(errorMessage(error)))
      .finally(() => setLoading(false));
  }, [api, message, path, settings.projectPath]);

  return (
    <Drawer
      width={760}
      title={path || "页面预览"}
      open={Boolean(path)}
      onClose={onClose}
      extra={
        path ? (
          <Text code copyable={{ text: path }}>
            {path}
          </Text>
        ) : null
      }
    >
      {loading ? (
        <Alert type="info" showIcon message="正在加载页面" />
      ) : content ? (
        <Space direction="vertical" size={12} className="page-stack">
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label="标题">{content.title || "-"}</Descriptions.Item>
            <Descriptions.Item label="类型">{content.type || content.kind || "-"}</Descriptions.Item>
            <Descriptions.Item label="更新时间">{formatDateTime(content.mod_time)}</Descriptions.Item>
          </Descriptions>
          <div className="markdown-view">{renderMarkdown(content.content, onOpenFile)}</div>
        </Space>
      ) : (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无内容" />
      )}
    </Drawer>
  );
}

function summarizeWikiPages(files: ProjectFile[]) {
  return files.reduce(
    (acc, file) => {
      if (file.path.startsWith("wiki/concepts/")) acc.concept += 1;
      else if (file.path.startsWith("wiki/entities/")) acc.entity += 1;
      else if (file.path.startsWith("wiki/syntheses/")) acc.synthesis += 1;
      else if (file.path.startsWith("wiki/sources/")) acc.source += 1;
      else acc.other += 1;
      return acc;
    },
    { concept: 0, entity: 0, synthesis: 0, source: 0, other: 0 },
  );
}

function WorkbenchPage({
  api,
  settings,
  initialPath,
}: {
  api: ApiClient;
  settings: AppSettings;
  initialPath: string;
}) {
  const { message } = AntApp.useApp();
  const [files, setFiles] = useState<ProjectFile[]>([]);
  const [selectedPath, setSelectedPath] = useState(initialPath || "wiki/index.md");
  const [content, setContent] = useState<ProjectFileContent | null>(null);
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [reviews, setReviews] = useState<ReviewItem[]>([]);
  const [filter, setFilter] = useState("");
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [fileData, graphData, reviewData] = await Promise.all([
        api.projectFiles(settings.projectPath),
        api.wikiGraph(settings.projectPath),
        api.reviews(settings.projectPath, settings.projectID),
      ]);
      setFiles(fileData.files ?? []);
		setGraph(normalizeGraphResponse(graphData));
      setReviews(reviewData.reviews ?? []);
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  }, [api, message, settings.projectID, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    if (initialPath) {
      setSelectedPath(initialPath);
    }
  }, [initialPath]);

  const openPath = useCallback(
    async (path: string) => {
      const resolved = resolveProjectPath(path, files);
      if (!resolved) {
        message.warning(`未找到页面：${path}`);
        return;
      }
      setSelectedPath(resolved);
      setLoading(true);
      try {
        const next = await api.projectFileContent(settings.projectPath, resolved);
        setContent(next);
        setDraft(next.content);
        setEditing(false);
      } catch (error) {
        message.error(errorMessage(error));
      } finally {
        setLoading(false);
      }
    },
    [api, files, message, settings.projectPath],
  );

  useEffect(() => {
    if (files.length > 0) {
      void openPath(selectedPath);
    }
  }, [files, openPath, selectedPath]);

  const filteredFiles = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return files;
    return files.filter((file) => file.path.toLowerCase().includes(q) || file.kind.toLowerCase().includes(q));
  }, [files, filter]);

  const backlinks = useMemo(() => {
    if (!graph) return [];
    return graph.edges.filter((edge) => edge.target === selectedPath).map((edge) => edge.source);
  }, [graph, selectedPath]);

  const relatedReviews = useMemo(
    () => reviews.filter((review) => (review.AffectedPages ?? []).includes(selectedPath)),
    [reviews, selectedPath],
  );
  const canEdit = content ? content.path === "purpose.md" || content.path === "schema.md" || content.path.startsWith("wiki/") : false;

  const saveContent = async () => {
    if (!content) return;
    setLoading(true);
    try {
      const result = await api.writeProjectFileContent({
        project_path: settings.projectPath,
        path: content.path,
        content: draft,
        reason: "workbench edit",
      });
      message.success(result.versioned ? "已保存并归档旧版本" : "已保存");
      await openPath(content.path);
      await refresh();
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="workbench-layout">
      <div className="workbench-sidebar">
        <Space direction="vertical" size={12} className="page-stack">
          <Input
            prefix={<SearchOutlined />}
            placeholder="筛选文件"
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
          />
          <Tree
            className="file-tree"
            treeData={buildFileTree(filteredFiles)}
            selectedKeys={[selectedPath]}
            onSelect={(keys) => {
              const key = String(keys[0] ?? "");
              if (key) void openPath(key);
            }}
          />
        </Space>
      </div>
      <div className="workbench-preview">
        <Space direction="vertical" size={12} className="page-stack">
          <div className="toolbar">
            <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>
              刷新
            </Button>
            <Text code copyable={{ text: selectedPath }}>
              {selectedPath}
            </Text>
            {content?.type && <Tag>{content.type}</Tag>}
            {canEdit && (
              <>
                <Button icon={<EditOutlined />} onClick={() => setEditing((value) => !value)}>
                  {editing ? "预览" : "编辑"}
                </Button>
                {editing && (
                  <Button type="primary" icon={<SaveOutlined />} loading={loading} onClick={saveContent}>
                    保存
                  </Button>
                )}
              </>
            )}
          </div>
          {content ? (
            editing ? (
              <Input.TextArea
                className="markdown-editor"
                value={draft}
                onChange={(event) => setDraft(event.target.value)}
                autoSize={{ minRows: 24 }}
              />
            ) : (
              <div className="markdown-view">
                {renderMarkdown(content.content, openPath)}
              </div>
            )
          ) : (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="请选择一个文件" />
          )}
        </Space>
      </div>
      <div className="workbench-context">
        <Space direction="vertical" size={12} className="page-stack">
          <Title level={4}>页面上下文</Title>
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label="标题">{content?.title || "-"}</Descriptions.Item>
            <Descriptions.Item label="类型">{content?.type || content?.kind || "-"}</Descriptions.Item>
            <Descriptions.Item label="大小">{content ? `${content.size} bytes` : "-"}</Descriptions.Item>
            <Descriptions.Item label="更新时间">{formatDateTime(content?.mod_time)}</Descriptions.Item>
          </Descriptions>
          <ContextPathList title="来源" paths={content?.sources ?? []} onOpen={openPath} />
          <ContextPathList title="反向链接" paths={backlinks} onOpen={openPath} />
          <div>
            <Text strong>相关审阅</Text>
            {relatedReviews.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无相关审阅" />
            ) : (
              <Space direction="vertical" size={6} className="page-stack">
                {relatedReviews.map((review) => (
                  <Tag key={review.ID} color={review.Status === "open" ? "gold" : "default"}>
                    {review.Type}: {review.Title}
                  </Tag>
                ))}
              </Space>
            )}
          </div>
          {content?.frontmatter && <JsonBlock value={content.frontmatter} />}
        </Space>
      </div>
    </div>
  );
}

function ContextPathList({
  title,
  paths,
  onOpen,
}: {
  title: string;
  paths: string[];
  onOpen: (path: string) => void;
}) {
  return (
    <div>
      <Text strong>{title}</Text>
      {paths.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无" />
      ) : (
        <Space direction="vertical" size={4} className="page-stack">
          {paths.map((path) => (
            <Button key={path} type="link" size="small" className="path-link" onClick={() => onOpen(path)}>
              {path}
            </Button>
          ))}
        </Space>
      )}
    </div>
  );
}

function QueryPage({
  api,
  settings,
  onOpenFile,
  initialQuestion,
}: {
  api: ApiClient;
  settings: AppSettings;
  onOpenFile: (path: string) => void;
  initialQuestion?: string;
}) {
  const { message, modal } = AntApp.useApp();
  const [sessions, setSessions] = useState<ChatSessionRecord[]>([]);
  const [sessionID, setSessionID] = useState("");
  const [question, setQuestion] = useState("");
  const agent: AgentName = "llm";
  const [limit, setLimit] = useState<number>(10);
  const [saveTitle, setSaveTitle] = useState("");
  const [loading, setLoading] = useState(false);
  const [activeRun, setActiveRun] = useState<ActiveChatRunState | null>(null);
  const [runClock, setRunClock] = useState(() => Date.now());
  const runAbortRef = useRef<AbortController | null>(null);

  const refreshSessions = useCallback(async () => {
    try {
      const data = await api.chats(settings.projectPath);
      setSessions(data.sessions ?? []);
      setSessionID((current) => current || data.sessions?.[0]?.id || "");
    } catch (error) {
      message.error(errorMessage(error));
    }
  }, [api, message, settings.projectPath]);

  useEffect(() => {
    void refreshSessions();
  }, [refreshSessions]);

  useEffect(() => {
    if (initialQuestion?.trim()) setQuestion(initialQuestion.trim());
  }, [initialQuestion]);

  useEffect(() => {
    if (activeRun?.status !== "running") {
      return;
    }
    const timer = window.setInterval(() => setRunClock(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [activeRun?.status]);

  const activeSession = sessions.find((session) => session.id === sessionID);
  const visibleRun = activeRun?.chatID === activeSession?.id ? activeRun : null;

  const ensureSession = async (seedQuestion: string): Promise<ChatSessionRecord> => {
    const existing = sessions.find((session) => session.id === sessionID);
    if (existing) return existing;
    const created = await api.createChat({
      project_path: settings.projectPath,
      title: seedQuestion.slice(0, 32) || "新的对话",
    });
    setSessions((current) => [created.session, ...current]);
    setSessionID(created.session.id);
    return created.session;
  };

  const submit = async () => {
    if (!question.trim()) {
      message.warning("请输入问题");
      return;
    }
    const currentQuestion = question.trim();
    const run = async () => {
      setLoading(true);
      try {
        const baseSession = await ensureSession(currentQuestion);
        setQuestion("");
        const started = await api.startChatRun({
          project_path: settings.projectPath,
          project_id: settings.projectID,
          chat_id: baseSession.id,
          q: currentQuestion,
          limit,
          agent,
          save_title: saveTitle.trim() || undefined,
        });
        setSessions((current) => upsertServerSession(current, started.session));
        setSessionID(started.session.id);
        const controller = new AbortController();
        runAbortRef.current = controller;
        setActiveRun({
          chatID: started.session.id,
          runID: started.run.id,
          question: currentQuestion,
          status: "running",
          startedAt: started.run.started_at,
          events: [],
        });
        await api.streamChatRunEvents({
          project_path: settings.projectPath,
          chat_id: started.session.id,
          run_id: started.run.id,
          signal: controller.signal,
          onEvent: (event) => {
            setActiveRun((current) => {
              if (!current || current.runID !== event.run_id) return current;
              return {
                ...current,
                status: terminalChatRunStatus(event.type) ?? current.status,
                events: upsertRunEvent(current.events, event),
              };
            });
            if (event.session) {
              setSessions((current) => upsertServerSession(current, event.session!));
            }
            if (event.type === "completed") {
              message.success(saveTitle.trim() ? "查询已保存" : "查询完成");
              setLoading(false);
            } else if (event.type === "error") {
              message.error(event.message || "查询失败");
              setLoading(false);
            } else if (event.type === "canceled") {
              message.info("查询已取消");
              setLoading(false);
            }
          },
        });
      } catch (error) {
        if (!isAbortError(error)) {
          message.error(errorMessage(error));
        }
      } finally {
        runAbortRef.current = null;
        setLoading(false);
      }
    };
    if (saveTitle.trim()) {
      modal.confirm({
        title: "保存综合页",
        content: saveTitle.trim(),
        icon: <CheckCircleOutlined />,
        onOk: run,
      });
      return;
    }
    await run();
  };

  const cancelRun = async () => {
    if (!activeRun || activeRun.status !== "running") return;
    try {
      await api.cancelChatRun(settings.projectPath, activeRun.chatID, activeRun.runID);
    } catch (error) {
      message.error(errorMessage(error));
    }
  };

  const newSession = () => {
    void api
      .createChat({ project_path: settings.projectPath, title: "新的对话" })
      .then((data) => {
        setSessions((current) => [data.session, ...current]);
        setSessionID(data.session.id);
        setQuestion("");
      })
      .catch((error) => message.error(errorMessage(error)));
  };

  const clearSession = () => {
    if (!sessionID) return;
    void api
      .deleteChat(settings.projectPath, sessionID)
      .then(() => {
        const next = sessions.filter((session) => session.id !== sessionID);
        setSessions(next);
        setSessionID(next[0]?.id ?? "");
      })
      .catch((error) => message.error(errorMessage(error)));
  };

  return (
    <div className="chat-layout">
      <div className="chat-sessions">
        <Button type="primary" icon={<MessageOutlined />} onClick={newSession}>
          新对话
        </Button>
        <Space direction="vertical" size={8} className="page-stack">
          {sessions.map((session) => (
            <Button
              key={session.id}
              className="session-button"
              type={session.id === sessionID ? "primary" : "default"}
              onClick={() => setSessionID(session.id)}
            >
              {session.title || "未命名对话"}
            </Button>
          ))}
        </Space>
      </div>
      <div className="chat-main">
        <div className="chat-history">
          {(activeSession?.messages ?? []).length === 0 ? (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="开始一次面向 Wiki 的查询" />
          ) : (
            activeSession?.messages.map((item) => (
              <ChatBubble key={item.id} message={item} onOpenFile={onOpenFile} />
            ))
          )}
          {visibleRun && <ChatRunBubble run={visibleRun} now={runClock} onCancel={cancelRun} />}
        </div>
        <div className="chat-composer panel">
          <Form layout="vertical">
            <Form.Item label="问题">
              <Input.TextArea
                rows={3}
                value={question}
                onChange={(event) => setQuestion(event.target.value)}
                onPressEnter={(event) => {
                  if (!event.shiftKey) {
                    event.preventDefault();
                    void submit();
                  }
                }}
              />
            </Form.Item>
            <Space wrap>
              <Tag color="blue">LLM</Tag>
              <InputNumber min={1} max={50} value={limit} onChange={(value) => setLimit(value ?? 10)} />
              <Input
                className="save-title-input"
                prefix={<SaveOutlined />}
                value={saveTitle}
                onChange={(event) => setSaveTitle(event.target.value)}
                placeholder="保存综合页标题（可选）"
              />
              <Button type="primary" icon={<SendOutlined />} loading={loading} onClick={submit}>
                发送
              </Button>
              <Button danger onClick={clearSession}>
                清空当前对话
              </Button>
            </Space>
          </Form>
        </div>
      </div>
    </div>
  );
}

type ActiveChatRunState = {
  chatID: string;
  runID: string;
  question: string;
  status: "running" | "succeeded" | "failed" | "canceled";
  startedAt: string;
  events: ChatRunEvent[];
};

function ChatRunBubble({
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

function ChatBubble({
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
              {
                key: "plan",
                label: "计划",
                children: <JsonBlock value={answer.plan} />,
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

function upsertRunEvent(events: ChatRunEvent[], event: ChatRunEvent): ChatRunEvent[] {
  if (events.some((item) => item.id === event.id)) return events;
  return [...events, event];
}

type RunStageStatus = "waiting" | "active" | "done" | "failed" | "canceled";

type RunStageSummary = {
  key: string;
  label: string;
  status: RunStageStatus;
};

type RunSummary = {
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

function summarizeChatRun(run: ActiveChatRunState, now: number): RunSummary {
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

function activeRunStage(status: ActiveChatRunState["status"], latestType?: string): string {
  if (status === "succeeded") return "done";
  if (status === "failed") return "done";
  if (status === "canceled") return "done";
  if (!latestType || latestType === "started") return "intake";
  if (latestType === "routing_started" || latestType === "routing_done") return "routing";
  if (latestType === "general_answer_started" || latestType === "general_answer_done") return "direct_answer";
  if (latestType === "planning_started" || latestType === "planning_done") return "planning";
  if (latestType === "synthesis_started" || latestType === "action_retry_exhausted") return "synthesis";
  if (latestType === "completed" || latestType === "error" || latestType === "canceled") return "done";
  return "evidence";
}

function buildRunStages(activeKey: string, status: ActiveChatRunState["status"], intent?: string): RunStageSummary[] {
  const order = isNonWikiRunIntent(intent)
    ? [
        { key: "intake", label: "接收问题" },
        { key: "routing", label: "判断意图" },
        { key: "direct_answer", label: "直接回答" },
        { key: "done", label: "完成" },
      ]
    : [
        { key: "intake", label: "接收问题" },
        { key: "routing", label: "判断意图" },
        { key: "planning", label: "规划查询" },
        { key: "evidence", label: "读取证据" },
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

function runTitle(status: ActiveChatRunState["status"], stage: string, latest?: ChatRunEvent): string {
  if (status === "succeeded") return "答案已生成";
  if (status === "failed") return latest?.message || "查询失败";
  if (status === "canceled") return "查询已取消";
  if (stage === "routing") return "正在判断问题意图";
  if (stage === "direct_answer") return "正在直接生成回答";
  if (stage === "planning") return "正在等待模型制定查询计划";
  if (stage === "evidence") return latest?.action?.action ? `正在执行 ${actionLabel(latest.action.action)}` : "正在收集证据";
  if (stage === "synthesis") return "正在等待模型综合答案";
  return "已收到问题，准备开始查询";
}

function runSubtitle(status: ActiveChatRunState["status"], elapsedMs: number, latest?: ChatRunEvent): string {
  if (status === "running") {
    const waiting = latest ? `当前：${eventTypeLabel(latest.type)}` : "等待第一条事件";
    return `${waiting} · 已等待 ${formatElapsed(elapsedMs)}`;
  }
  if (status === "succeeded") return `总耗时 ${formatElapsed(elapsedMs)} · 执行过程已折叠`;
  if (status === "canceled") return `在 ${formatElapsed(elapsedMs)} 后取消，未写入答案`;
  return `在 ${formatElapsed(elapsedMs)} 后结束`;
}

function stagePercent(stage: string): number {
  const values: Record<string, number> = {
    intake: 12,
    routing: 26,
    planning: 42,
    evidence: 68,
    synthesis: 88,
    direct_answer: 78,
    done: 100,
  };
  return values[stage] ?? 12;
}

function routeIntentFromEvents(events: ChatRunEvent[]): string {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const match = events[i].observation?.match(/intent=([a-z_]+)/);
    if (match?.[1]) return match[1];
  }
  return "";
}

function isNonWikiRunIntent(intent?: string): boolean {
  return ["direct_chat", "system_faq", "general_assistant", "missing_evidence", "unsupported"].includes(intent ?? "");
}

function countRunActions(events: ChatRunEvent[], actions: string[]): number {
  return events.filter((event) => event.type === "action_done" && event.action?.action && actions.includes(event.action.action)).length;
}

function actionLabel(action: string): string {
  const labels: Record<string, string> = {
    read: "读取页面",
    list_pages: "浏览目录",
    list: "浏览目录",
    follow_links: "展开链接",
    search: "搜索页面",
    graph: "查询图谱",
    expand: "查询图谱",
    final: "生成答案",
    writeback: "准备写回",
  };
  return labels[action] ?? action;
}

function formatElapsed(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return `${minutes}m ${rest}s`;
}

function terminalChatRunStatus(type: string): ActiveChatRunState["status"] | null {
  if (type === "completed") return "succeeded";
  if (type === "error") return "failed";
  if (type === "canceled") return "canceled";
  return null;
}

function eventTypeLabel(type: string): string {
  const labels: Record<string, string> = {
    started: "启动",
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
    action_retry_exhausted: "降级综合",
    writeback_done: "写回",
    completed: "完成",
    error: "失败",
    canceled: "取消",
  };
  return labels[type] ?? type;
}

function intentLabel(intent: string): string {
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

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

function GraphPage({
  api,
  settings,
  onOpenFile,
}: {
  api: ApiClient;
  settings: AppSettings;
  onOpenFile: (path: string) => void;
}) {
  const { message } = AntApp.useApp();
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [insights, setInsights] = useState<WikiGraphInsightsResponse | null>(null);
  const [researchJobs, setResearchJobs] = useState<ResearchJob[]>([]);
	const [graphJobs, setGraphJobs] = useState<GraphIndexJob[]>([]);
	const [repositories, setRepositories] = useState<GraphRepositoryStatus[]>([]);
	const [query, setQuery] = useState("");
	const [domainFilter, setDomainFilter] = useState<string[]>([]);
	const [kindFilter, setKindFilter] = useState<string[]>([]);
	const [relationFilter, setRelationFilter] = useState<string[]>([]);
	const [confidenceFilter, setConfidenceFilter] = useState<string[]>([]);
	const [selectedNodeID, setSelectedNodeID] = useState<string | null>(null);
	const [selectedNode, setSelectedNode] = useState<WikiGraphResponse["nodes"][number] | null>(null);
	const [selectedEdges, setSelectedEdges] = useState<WikiGraphResponse["edges"]>([]);
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
		const [graphData, insightData, jobsData, graphJobsData, repositoriesData] = await Promise.all([
			api.projectGraph({
				project_path: settings.projectPath,
				query,
				domains: domainFilter,
				kinds: kindFilter,
				relations: relationFilter,
				confidence: confidenceFilter,
				limit: 250,
			}),
        api.wikiGraphInsights(settings.projectPath),
        api.researchJobs(settings.projectPath),
			api.graphJobs(),
			api.graphRepositories(),
      ]);
      setGraph(graphData);
      setInsights(insightData);
      setResearchJobs(jobsData.jobs ?? []);
		setGraphJobs(graphJobsData.jobs ?? []);
		setRepositories(repositoriesData.repositories ?? []);
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
	}, [api, confidenceFilter, domainFilter, kindFilter, message, query, relationFilter, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const nodeTypes = useMemo(() => {
		const values = Array.from(new Set((graph?.nodes ?? []).map((node) => node.kind || node.type).filter(Boolean))).sort();
		return values.map((value) => ({ value, label: value }));
  }, [graph]);

	const relationTypes = useMemo(
		() => Array.from(new Set((graph?.edges ?? []).map((edge) => edge.relation || edge.kind).filter(Boolean))).sort().map((value) => ({ value, label: value })),
		[graph],
	);

	const selectAndExpandNode = useCallback(async (nodeID: string) => {
		setSelectedNodeID(nodeID);
		try {
			const [detail, expansion] = await Promise.all([
				api.graphNode(settings.projectPath, nodeID),
				api.projectGraph({
					project_path: settings.projectPath,
					seed_ids: [nodeID],
					depth: 1,
					limit: 250,
					domains: domainFilter,
					kinds: kindFilter,
					relations: relationFilter,
					confidence: confidenceFilter,
				}),
			]);
			setSelectedNode(detail.node);
			setSelectedEdges(detail.edges);
			setGraph((current) => mergeGraphResponses(current, expansion));
		} catch (error) {
			message.error(errorMessage(error));
		}
	}, [api, confidenceFilter, domainFilter, kindFilter, message, relationFilter, settings.projectPath]);

	const queueRepository = async (repository: GraphRepositoryStatus) => {
		setLoading(true);
		try {
			const response = await api.queueGraphJob({ registry_id: repository.registry_id, repository_id: repository.repository_id, branch: repository.branch });
			setGraphJobs((current) => [response.job, ...current.filter((job) => job.id !== response.job.id)]);
			message.success("索引任务已进入后台队列");
		} catch (error) {
			message.error(errorMessage(error));
		} finally {
			setLoading(false);
		}
	};

  const startResearch = async (insight: WikiGraphInsight) => {
    setLoading(true);
    try {
      const response = await api.createResearchJob({
        project_path: settings.projectPath,
        project_id: settings.projectID,
        topic: insight.title || insight.path,
        query: insight.query || insight.title || insight.path,
        agent: settings.agent,
      });
      setResearchJobs((current) => [response.job, ...current.filter((job) => job.id !== response.job.id)]);
      message.success(response.existing ? "研究任务已在运行" : "研究任务已启动");
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="toolbar">
		<Input.Search value={query} onChange={(event) => setQuery(event.target.value)} onSearch={() => void refresh()} placeholder="搜索节点、路径或符号" className="graph-search" allowClear />
        <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>
			应用筛选
        </Button>
		<Select mode="multiple" value={domainFilter} onChange={setDomainFilter} options={[{ value: "wiki", label: "Wiki" }, { value: "source", label: "来源" }, { value: "code", label: "代码" }]} placeholder="领域" className="graph-filter" maxTagCount="responsive" />
		<Select mode="multiple" value={kindFilter} onChange={setKindFilter} options={nodeTypes} placeholder="节点类型" className="graph-filter" maxTagCount="responsive" />
		<Select mode="multiple" value={relationFilter} onChange={setRelationFilter} options={relationTypes} placeholder="关系" className="graph-filter" maxTagCount="responsive" />
		<Select mode="multiple" value={confidenceFilter} onChange={setConfidenceFilter} options={["EXTRACTED", "INFERRED", "AMBIGUOUS"].map((value) => ({ value, label: value }))} placeholder="置信度" className="graph-filter" />
		<Tag>节点 {graph?.nodes?.length ?? 0}/{graph?.stats?.total_nodes ?? graph?.nodes?.length ?? 0}</Tag>
		<Tag>边 {graph?.edges?.length ?? 0}/{graph?.stats?.total_edges ?? graph?.edges?.length ?? 0}</Tag>
		{graph?.truncated && <Tag color="gold">按需加载</Tag>}
      </div>
      <div className="graph-layout">
        <div className="graph-canvas panel">
			<UnifiedGraphCanvas graph={graph} selectedNodeID={selectedNodeID} onSelectNode={(id) => void selectAndExpandNode(id)} />
        </div>
        <div className="panel graph-list">
          <Tabs
            items={[
              {
                key: "nodes",
				label: "节点",
				children: <GraphNodeTable nodes={graph?.nodes ?? []} onSelectNode={(id) => void selectAndExpandNode(id)} onOpenFile={onOpenFile} />,
			},
			{
				key: "detail",
				label: "详情",
				children: selectedNode ? (
					<Space direction="vertical" className="page-stack">
						<Descriptions size="small" column={1} items={[
							{ key: "label", label: "节点", children: selectedNode.label || selectedNode.title },
							{ key: "domain", label: "领域", children: <Tag>{selectedNode.domain}</Tag> },
							{ key: "kind", label: "类型", children: <Tag>{selectedNode.kind}</Tag> },
							{ key: "path", label: "路径", children: <Text code>{selectedNode.path || selectedNode.source_ref || "-"}</Text> },
							{ key: "community", label: "社区", children: selectedNode.community || "-" },
							{ key: "degree", label: "连接", children: `${selectedNode.in_degree} 入 / ${selectedNode.out_degree} 出` },
						]} />
						{(selectedNode.domain === "wiki" || selectedNode.domain === "source") && selectedNode.path && <Button onClick={() => onOpenFile(selectedNode.path)}>打开证据文件</Button>}
						<Table rowKey="id" size="small" pagination={{ pageSize: 8 }} dataSource={selectedEdges} columns={[
							{ title: "关系", dataIndex: "relation", render: (value, edge) => <Tooltip title={`score=${edge.confidence_score}`}><Tag color={confidenceColor(edge.confidence)}>{value}</Tag></Tooltip> },
							{ title: "方向", render: (_, edge) => edge.source === selectedNode.id ? `→ ${edge.target}` : `← ${edge.source}` },
						]} />
					</Space>
				) : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="点击节点查看并扩展一层关系" />,
			},
			{
				key: "repositories",
				label: `仓库 ${repositories.length}`,
				children: <GraphRepositoriesPanel repositories={repositories} jobs={graphJobs} loading={loading} onQueue={queueRepository} />,
              },
              {
                key: "insights",
                label: "洞察",
                children: (
                  <GraphInsightsPanel
                    insights={insights}
                    onOpenFile={onOpenFile}
                    onResearch={startResearch}
                  />
                ),
              },
              {
                key: "research",
                label: "研究任务",
                children: <ResearchJobsPanel jobs={researchJobs} onOpenFile={onOpenFile} />,
              },
            ]}
          />
        </div>
      </div>
    </Space>
  );
}

function GraphNodeTable({
  nodes,
	onSelectNode,
  onOpenFile,
}: {
  nodes: WikiGraphResponse["nodes"];
	onSelectNode: (id: string) => void;
  onOpenFile: (path: string) => void;
}) {
  return (
    <Table
		rowKey="id"
      size="small"
      pagination={{ pageSize: 12 }}
      columns={[
        {
			title: "节点",
          dataIndex: "title",
          render: (_, node) => (
			<Button type="link" className="path-link" onClick={() => onSelectNode(node.id)}>
				{node.label || node.title || node.path || node.id}
            </Button>
          ),
        },
		{ title: "领域", dataIndex: "domain", width: 78, render: (value) => <Tag color={domainColor(value)}>{value}</Tag> },
		{ title: "类型", dataIndex: "kind", width: 110, render: (value) => <Tag>{value}</Tag> },
        { title: "入", dataIndex: "in_degree", width: 60 },
        { title: "出", dataIndex: "out_degree", width: 60 },
		{ title: "证据", width: 70, render: (_, node) => (node.domain === "wiki" || node.domain === "source") && node.path ? <Button size="small" type="text" onClick={() => onOpenFile(node.path)}><EyeOutlined /></Button> : null },
      ]}
      dataSource={nodes}
    />
  );
}

function GraphRepositoriesPanel({
	repositories,
	jobs,
	loading,
	onQueue,
}: {
	repositories: GraphRepositoryStatus[];
	jobs: GraphIndexJob[];
	loading: boolean;
	onQueue: (repository: GraphRepositoryStatus) => void;
}) {
	return (
		<Space direction="vertical" className="page-stack">
			<Table rowKey={(repo) => `${repo.registry_id}:${repo.repository_id}`} size="small" pagination={false} dataSource={repositories} columns={[
				{ title: "仓库", render: (_, repo) => <Space direction="vertical" size={0}><Text strong>{repo.full_name}</Text><Text type="secondary">{repo.provider} · {repo.branch}{repo.commit ? ` · ${repo.commit.slice(0, 12)}` : ""}</Text></Space> },
				{ title: "状态", width: 150, render: (_, repo) => <Space size={4}>{repo.indexed ? <Tag color="green">已索引</Tag> : <Tag>未索引</Tag>}{repo.dirty ? <Tag color="orange" title={repo.source_sha256}>含未提交改动</Tag> : null}</Space> },
				{ title: "操作", width: 90, render: (_, repo) => <Button size="small" loading={loading} disabled={repo.disabled} onClick={() => onQueue(repo)}>重新索引</Button> },
			]} />
			<Table rowKey="id" size="small" pagination={{ pageSize: 6 }} dataSource={jobs} columns={[
				{ title: "任务", render: (_, job) => `${job.repository_id} · ${job.trigger}` },
				{ title: "状态", dataIndex: "status", width: 100, render: (value) => statusTag(value) },
				{ title: "规模", width: 90, render: (_, job) => job.node_count ? `${job.node_count}/${job.edge_count}` : "-" },
				{ title: "错误", dataIndex: "error", ellipsis: true },
			]} />
		</Space>
	);
}

function GraphInsightsPanel({
  insights,
  onOpenFile,
  onResearch,
}: {
  insights: WikiGraphInsightsResponse | null;
  onOpenFile: (path: string) => void;
  onResearch: (insight: WikiGraphInsight) => void;
}) {
  const groups: Array<[string, WikiGraphInsight[]]> = [
    ["孤立页面", insights?.isolated_pages ?? []],
    ["缺少来源", insights?.missing_sources ?? []],
    ["桥接候选", insights?.bridge_candidates ?? []],
    ["高连接页面", insights?.hub_pages ?? []],
		["代码 God Nodes", insights?.god_nodes ?? []],
		["跨社区连接", insights?.surprising_connections ?? []],
  ];
  return (
    <Space direction="vertical" size={12} className="page-stack">
      {groups.map(([title, items]) => (
        <div key={title}>
          <Text strong>{title}</Text>
          {items.length === 0 ? (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无" />
          ) : (
            <Space direction="vertical" size={8} className="page-stack">
              {items.slice(0, 8).map((item) => (
				<div key={`${title}:${item.id || item.path || item.title}`} className="insight-row">
					<Button type="link" className="path-link" disabled={!item.path || item.domain === "code"} onClick={() => item.path && onOpenFile(item.path)}>
                    {item.title || item.path}
                  </Button>
                  <Text type="secondary">{item.reason}</Text>
                  <Button size="small" onClick={() => onResearch(item)}>
                    发起研究
                  </Button>
                </div>
              ))}
            </Space>
          )}
        </div>
      ))}
		{(insights?.suggested_questions?.length ?? 0) > 0 && (
			<div>
				<Text strong>建议问题</Text>
				<ul>{(insights?.suggested_questions ?? []).slice(0, 10).map((question) => <li key={question}><Text>{question}</Text></li>)}</ul>
			</div>
		)}
    </Space>
  );
}

function ResearchJobsPanel({
  jobs,
  onOpenFile,
}: {
  jobs: ResearchJob[];
  onOpenFile: (path: string) => void;
}) {
  return (
    <Table
      rowKey="id"
      size="small"
      pagination={{ pageSize: 8 }}
      columns={[
        { title: "主题", render: (_, job) => job.request.topic || job.request.query || job.request.review_id || job.id },
        { title: "状态", dataIndex: "status", width: 100, render: (value) => statusTag(value) },
        {
          title: "产物",
          dataIndex: "written_paths",
          render: (paths?: string[]) =>
            (paths ?? []).map((path) => (
              <Button key={path} type="link" className="path-link" onClick={() => onOpenFile(path)}>
                {path}
              </Button>
            )),
        },
        { title: "错误", dataIndex: "error", ellipsis: true },
      ]}
      dataSource={jobs}
    />
  );
}

function UnifiedGraphCanvas({
  graph,
  selectedNodeID,
  onSelectNode,
}: {
  graph: WikiGraphResponse | null;
  selectedNodeID: string | null;
  onSelectNode: (id: string) => void;
}) {
  const containerRef = useRef<HTMLDivElement | null>(null);

	useEffect(() => {
		const container = containerRef.current;
		if (!container || !graph || !Array.isArray(graph.nodes) || graph.nodes.length === 0) return;
		let instance: G6Graph | null = null;
		let cancelled = false;
		const nodeData: NodeData[] = graph.nodes.map((node) => ({
      id: node.id,
      data: { ...node },
    }));
    const edgeData: EdgeData[] = graph.edges.map((edge) => ({
      id: edge.id || `${edge.source}:${edge.target}:${edge.relation}`,
      source: edge.source,
      target: edge.target,
      data: { ...edge },
    }));
		void import("@antv/g6").then(({ Graph, NodeEvent }) => {
			if (cancelled) return;
			instance = new Graph({
      container,
      width: Math.max(container.clientWidth, 640),
      height: Math.max(container.clientHeight, 620),
      autoFit: "view",
      animation: false,
      data: { nodes: nodeData, edges: edgeData },
      layout: {
        type: "d3-force",
        animation: false,
        preventOverlap: true,
        manyBody: { strength: -260 },
        link: { distance: 110, strength: 0.8 },
      },
      node: {
        style: (datum) => {
          const node = datum.data as unknown as WikiGraphResponse["nodes"][number];
          const degree = (node.in_degree ?? 0) + (node.out_degree ?? 0);
          const selected = datum.id === selectedNodeID;
          return {
            size: Math.max(18, Math.min(46, 18 + degree * 1.7)),
            fill: domainFill(node.domain),
            stroke: selected ? "#f59e0b" : domainStroke(node.domain),
            lineWidth: selected ? 4 : 2,
            labelText: truncateLabel(node.label || node.title || node.id, 22),
            labelPlacement: "bottom",
            labelFill: "#17324d",
            labelFontSize: 11,
            labelBackground: true,
            labelBackgroundFill: "rgba(255,255,255,0.86)",
            labelBackgroundRadius: 3,
            cursor: "pointer",
          };
        },
      },
      edge: {
        style: (datum) => {
          const edge = datum.data as unknown as WikiGraphResponse["edges"][number];
          return {
            stroke: confidenceStroke(edge.confidence),
            strokeOpacity: edge.confidence === "AMBIGUOUS" ? 0.38 : 0.68,
            lineWidth: edge.confidence === "EXTRACTED" ? 1.6 : 1,
            lineDash: edge.confidence === "EXTRACTED" ? undefined : [4, 4],
            endArrow: true,
            endArrowSize: 5,
          };
        },
      },
      behaviors: ["drag-canvas", "zoom-canvas", "drag-element", "click-select"],
			});
			instance.on(NodeEvent.CLICK, (event: IPointerEvent) => {
				const id = (event.target as { id?: string }).id;
				if (id) onSelectNode(id);
			});
			void instance.render();
		});
		return () => {
			cancelled = true;
			instance?.destroy();
		};
  }, [graph, onSelectNode, selectedNodeID]);

	if (!graph || !Array.isArray(graph.nodes) || graph.nodes.length === 0) {
    return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无图谱数据" />;
  }
  return <div ref={containerRef} className="unified-graph-canvas" role="img" aria-label="统一知识图谱" />;
}

function mergeGraphResponses(current: WikiGraphResponse | null, incoming: WikiGraphResponse): WikiGraphResponse {
	incoming = normalizeGraphResponse(incoming);
	if (!current) return incoming;
  const nodes = new Map(current.nodes.map((node) => [node.id, node]));
  const edges = new Map(current.edges.map((edge) => [edge.id || `${edge.source}:${edge.target}:${edge.relation}`, edge]));
  incoming.nodes.forEach((node) => nodes.set(node.id, node));
  incoming.edges.forEach((edge) => edges.set(edge.id || `${edge.source}:${edge.target}:${edge.relation}`, edge));
  return {
    ...current,
    ...incoming,
    nodes: Array.from(nodes.values()),
    edges: Array.from(edges.values()),
    stats: incoming.stats ?? current.stats,
  };
}

function normalizeGraphResponse(graph: WikiGraphResponse): WikiGraphResponse {
  return { ...graph, nodes: graph.nodes ?? [], edges: graph.edges ?? [] };
}

function domainFill(domain: string): string {
  return { wiki: "#dbeafe", source: "#fef3c7", code: "#d1fae5" }[domain] ?? "#e5e7eb";
}

function domainStroke(domain: string): string {
  return { wiki: "#2563eb", source: "#d97706", code: "#059669" }[domain] ?? "#64748b";
}

function domainColor(domain: string): string {
  return { wiki: "blue", source: "gold", code: "green" }[domain] ?? "default";
}

function confidenceStroke(confidence: string): string {
  return { EXTRACTED: "#64748b", INFERRED: "#7c3aed", AMBIGUOUS: "#d97706" }[confidence] ?? "#94a3b8";
}

function confidenceColor(confidence: string): string {
  return { EXTRACTED: "green", INFERRED: "purple", AMBIGUOUS: "orange" }[confidence] ?? "default";
}

function SourcesPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message, modal } = AntApp.useApp();
  const [sourcePath, setSourcePath] = useState("");
  const [title, setTitle] = useState("");
  const [queue, setQueue] = useState<IngestTask[]>([]);
  const [sources, setSources] = useState<SourceManifestEntry[]>([]);
  const [lastRun, setLastRun] = useState<RunQueueResult | ScanSourcesResult | DeleteSourceResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [keepDone, setKeepDone] = useState(true);
  const [autoScan, setAutoScan] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const [queueData, sourcesData] = await Promise.all([
        api.queueTasks(settings.projectPath),
        api.sources(settings.projectPath),
      ]);
      setQueue(queueData.tasks ?? []);
      setSources(sourcesData.sources ?? []);
    } catch (error) {
      message.error(errorMessage(error));
    }
  }, [api, message, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    if (!autoScan) return;
    const timer = window.setInterval(() => {
      void scanSources(false);
    }, 15000);
    return () => window.clearInterval(timer);
  }, [autoScan, settings.projectPath]);

  const queueSource = async () => {
    if (!sourcePath.trim()) {
      message.warning("请输入来源路径");
      return;
    }
    setLoading(true);
    try {
      await api.queueSource({
        project_path: settings.projectPath,
        source_path: sourcePath,
        title,
      });
      setSourcePath("");
      setTitle("");
      await refresh();
      message.success("已加入队列");
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  async function scanSources(showToast = true) {
    setLoading(true);
    try {
      const data = await api.scanSources(settings.projectPath);
      setLastRun(data);
      await refresh();
      if (showToast) message.success("扫描完成");
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  }

  const runQueue = () => {
    modal.confirm({
      title: "运行摄取队列",
      icon: <SyncOutlined />,
      onOk: async () => {
        setLoading(true);
        try {
          const data = await api.runQueue({
            project_path: settings.projectPath,
            project_id: settings.projectID,
            agent: settings.agent,
            skip_unchanged: true,
            keep_done: keepDone,
          });
          setLastRun(data);
          await refresh();
          message.success("队列运行完成");
        } catch (error) {
          message.error(errorMessage(error));
        } finally {
          setLoading(false);
        }
      },
    });
  };

	const migrateLegacySources = async () => {
		setLoading(true);
		try {
			const plan = await api.sourceLayoutPlan(settings.projectPath);
			if (plan.total === 0) {
				message.success("来源已经使用逐来源归档布局");
				return;
			}
			modal.confirm({
				title: "迁移旧来源布局",
				content: `检测到 ${plan.total} 个旧来源。迁移会更新 manifest、Wiki 引用、队列和监视状态，不会调用 LLM。`,
				okText: "开始迁移",
				onOk: async () => {
					const result = await api.migrateSourceLayout(settings.projectPath);
					await refresh();
					message.success(`已迁移 ${result.completed}/${result.total} 个来源`);
				},
			});
		} catch (error) {
			message.error(errorMessage(error));
		} finally {
			setLoading(false);
		}
	};

  const deleteSource = async (source: SourceManifestEntry) => {
    setLoading(true);
    try {
      const preview = await api.deleteSource({
        project_path: settings.projectPath,
        project_id: settings.projectID,
        source_path: source.raw_path,
        dry_run: true,
      });
      setLastRun(preview);
      modal.confirm({
        title: "删除来源",
        content: (
          <Space direction="vertical" size={8}>
            <Text>{source.title || source.raw_path}</Text>
            <Text type="secondary">
              将删除 {preview.deleted_pages?.length ?? 0} 个页面，更新 {preview.updated_pages?.length ?? 0} 个页面，清理 {preview.cleaned_pages?.length ?? 0} 个页面。
            </Text>
          </Space>
        ),
        okText: "确认删除",
        okButtonProps: { danger: true },
        onOk: async () => {
          const result = await api.deleteSource({
            project_path: settings.projectPath,
            project_id: settings.projectID,
            source_path: source.raw_path,
          });
          setLastRun(result);
          await refresh();
          message.success("来源已删除");
        },
      });
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="来源路径">
            <Input value={sourcePath} onChange={(event) => setSourcePath(event.target.value)} />
          </Form.Item>
          <Form.Item label="标题">
            <Input value={title} onChange={(event) => setTitle(event.target.value)} />
          </Form.Item>
          <Space wrap>
            <Button type="primary" loading={loading} onClick={queueSource}>
              加入队列
            </Button>
            <Button icon={<ReloadOutlined />} loading={loading} onClick={() => scanSources()}>
              扫描来源
            </Button>
            <Button icon={<PlayCircleOutlined />} loading={loading} onClick={runQueue}>
              运行队列
            </Button>
			<Button icon={<FolderOpenOutlined />} loading={loading} onClick={() => void migrateLegacySources()}>
				迁移旧布局
			</Button>
            <Switch checked={keepDone} onChange={setKeepDone} />
            <Text>保留已完成任务</Text>
            <Switch checked={autoScan} onChange={setAutoScan} />
            <Text>自动扫描来源</Text>
          </Space>
        </Form>
      </div>
      {isScanSourcesResult(lastRun) && (lastRun.unsupported?.length ?? 0) > 0 && (
        <Alert
          type="warning"
          showIcon
          message={`有 ${lastRun.unsupported?.length ?? 0} 个文件未入队`}
          description={
            <Space direction="vertical" size={4}>
              {(lastRun.unsupported ?? []).map((item) => (
                <Text key={item.path} code>
                  {item.path}: {item.reason}
                </Text>
              ))}
            </Space>
          }
        />
      )}
      {isScanSourcesResult(lastRun) && (lastRun.events?.length ?? 0) > 0 && (
        <div className="panel">
          <Title level={4}>来源变化</Title>
          <Table
            rowKey={(item) => `${item.kind}:${item.path}`}
            size="small"
            pagination={false}
            columns={[
              { title: "类型", dataIndex: "kind", width: 120, render: (value) => statusTag(value) },
              { title: "路径", dataIndex: "path", ellipsis: true },
              { title: "说明", dataIndex: "reason", ellipsis: true },
            ]}
            dataSource={lastRun.events ?? []}
          />
        </div>
      )}
      <TaskTable tasks={queue} />
      <SourceManifestTable sources={sources} loading={loading} onDelete={deleteSource} />
      {lastRun && <JsonBlock value={lastRun} />}
    </Space>
  );
}

function ReviewsPage({
  api,
  settings,
  onOpenFile,
}: {
  api: ApiClient;
  settings: AppSettings;
  onOpenFile: (path: string) => void;
}) {
  const { message, modal } = AntApp.useApp();
  const [status, setStatus] = useState("open");
  const [data, setData] = useState<ReviewsResponse | null>(null);
  const [selectedReview, setSelectedReview] = useState<ReviewItem | null>(null);
  const [loading, setLoading] = useState(false);
  const [actionLoading, setActionLoading] = useState<string>("");
  const [lastAction, setLastAction] = useState<unknown>(null);
  const [researchJobs, setResearchJobs] = useState<ResearchJob[]>([]);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [reviewData, jobsData] = await Promise.all([
        api.reviews(settings.projectPath, settings.projectID),
        api.researchJobs(settings.projectPath),
      ]);
      setData(reviewData);
      setResearchJobs(jobsData.jobs ?? []);
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  }, [api, message, settings.projectID, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const reviews = data?.reviews ?? [];
  const counts = useMemo(
    () => ({
      open: reviews.filter((review) => review.Status === "open").length,
      resolved: reviews.filter((review) => review.Status === "resolved").length,
      dismissed: reviews.filter((review) => review.Status === "dismissed").length,
    }),
    [reviews],
  );
  const filteredReviews = useMemo(
    () => reviews.filter((review) => review.Status === status),
    [reviews, status],
  );

  const updateStatus = (review: ReviewItem, nextStatus: "resolved" | "dismissed" | "open") => {
    const action =
      nextStatus === "resolved" ? "resolve" : nextStatus === "dismissed" ? "dismiss" : "reopen";
    modal.confirm({
      title: nextStatus === "open" ? "重新打开审阅任务" : `${statusLabel(nextStatus)}审阅任务`,
      content: review.Title,
      onOk: async () => {
        try {
          await api.resolveReview({
            project_path: settings.projectPath,
            project_id: settings.projectID,
            id: review.ID,
            status: nextStatus,
            action,
          });
          await refresh();
          setSelectedReview((current) =>
            current?.ID === review.ID ? { ...current, Status: nextStatus } : current,
          );
          message.success("审阅已更新");
        } catch (error) {
          message.error(errorMessage(error));
        }
      },
    });
  };

  const runReviewAction = (review: ReviewItem, action: "create-page" | "deep-research") => {
    const title = action === "create-page" ? "生成页面草稿" : "启动深度研究";
    modal.confirm({
      title,
      content: review.Title,
      onOk: async () => {
        setActionLoading(`${review.ID}:${action}`);
        try {
          const result =
            action === "deep-research"
              ? await api.createResearchJob({
                  project_path: settings.projectPath,
                  project_id: settings.projectID,
                  review_id: review.ID,
                  agent: settings.agent,
                })
              : await api.reviewAction({
                  project_path: settings.projectPath,
                  project_id: settings.projectID,
                  id: review.ID,
                  action,
                  agent: settings.agent,
                });
          setLastAction(result);
          await refresh();
          if ("review" in result) {
            setSelectedReview((current) =>
              current?.ID === review.ID ? { ...current, Status: result.review.Status } : current,
            );
            message.success(result.message || `${title}完成`);
          } else {
            setResearchJobs((current) => [result.job, ...current.filter((job) => job.id !== result.job.id)]);
            message.success(result.existing ? "研究任务已在运行" : "研究任务已启动");
          }
        } catch (error) {
          message.error(errorMessage(error));
        } finally {
          setActionLoading("");
        }
      },
    });
  };

  const sweepReviews = () => {
    modal.confirm({
      title: "清理过期审阅任务",
      content: "系统会先用规则判断，再用 LLM 保守判断剩余任务。",
      onOk: async () => {
        setLoading(true);
        try {
          const result = await api.sweepReviews({
            project_path: settings.projectPath,
            project_id: settings.projectID,
            agent: settings.agent,
          });
          setData({ reviews: result.reviews ?? [], count: result.reviews?.length ?? 0 });
          setLastAction(result);
          message.success(`清理完成：规则 ${result.rule_resolved}，LLM ${result.llm_resolved}`);
        } catch (error) {
          message.error(errorMessage(error));
        } finally {
          setLoading(false);
        }
      },
    });
  };

  const columns: TableColumnsType<ReviewItem> = [
    {
      title: "任务",
      dataIndex: "Title",
      render: (_, review) => (
        <Space direction="vertical" size={4} className="review-title-cell">
          <Text strong>{review.Title}</Text>
          <Text type="secondary" ellipsis>
            {review.Description || "暂无详情"}
          </Text>
        </Space>
      ),
    },
    { title: "类型", dataIndex: "Type", width: 150, render: (value) => <Tag>{value}</Tag> },
    { title: "状态", dataIndex: "Status", width: 120, render: (value) => statusTag(value) },
    {
      title: "创建时间",
      dataIndex: "CreatedAt",
      width: 170,
      render: (value) => formatDateTime(value),
    },
    {
      title: "相关页面",
      dataIndex: "AffectedPages",
      render: (pages: string[]) => <ReviewPagePaths pages={pages ?? []} />,
    },
    {
      title: "操作",
      width: 260,
      render: (_, review) => {
        const isOpen = review.Status === "open";
        return (
          <Space>
            <Button size="small" icon={<EyeOutlined />} onClick={() => setSelectedReview(review)}>
              详情
            </Button>
            {isOpen ? (
              <>
                {review.Type === "missing-page" && (
                  <Button
                    size="small"
                    loading={actionLoading === `${review.ID}:create-page`}
                    onClick={() => runReviewAction(review, "create-page")}
                  >
                    生成草稿
                  </Button>
                )}
                {canDeepResearch(review) && (
                  <Button
                    size="small"
                    loading={actionLoading === `${review.ID}:deep-research`}
                    onClick={() => runReviewAction(review, "deep-research")}
                  >
                    深度研究
                  </Button>
                )}
                <Button size="small" type="primary" onClick={() => updateStatus(review, "resolved")}>
                  解决
                </Button>
                <Button size="small" danger onClick={() => updateStatus(review, "dismissed")}>
                  忽略
                </Button>
              </>
            ) : (
              <Button
                size="small"
                icon={<RollbackOutlined />}
                onClick={() => updateStatus(review, "open")}
              >
                重新打开
              </Button>
            )}
          </Space>
        );
      },
    },
  ];

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="metric-grid">
        <div className="metric-panel">
          <Statistic title="未处理任务" value={counts.open} />
        </div>
        <div className="metric-panel">
          <Statistic title="已解决任务" value={counts.resolved} />
        </div>
        <div className="metric-panel">
          <Statistic title="已忽略任务" value={counts.dismissed} />
        </div>
      </div>
      <div className="toolbar">
        <Select
          value={status}
          onChange={setStatus}
          options={[
            { value: "open", label: "未处理" },
            { value: "resolved", label: "已解决" },
            { value: "dismissed", label: "已忽略" },
          ]}
        />
        <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>
          刷新
        </Button>
        <Button loading={loading} onClick={sweepReviews}>
          清理过期任务
        </Button>
      </div>
      <Table
        rowKey="ID"
        columns={columns}
        dataSource={filteredReviews}
        loading={loading}
        locale={{
          emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无审阅任务" />,
        }}
      />
      <ReviewDetailDrawer
        review={selectedReview}
        onClose={() => setSelectedReview(null)}
        onUpdateStatus={updateStatus}
        onRunAction={runReviewAction}
        actionLoading={actionLoading}
      />
      <div className="panel">
        <Title level={4}>研究任务</Title>
        <ResearchJobsPanel jobs={researchJobs} onOpenFile={onOpenFile} />
      </div>
      {lastAction !== null && <JsonBlock value={lastAction} />}
    </Space>
  );
}

function ReviewPagePaths({ pages }: { pages: string[] }) {
  if (pages.length === 0) {
    return <Text type="secondary">-</Text>;
  }
  const visible = pages.slice(0, 3);
  return (
    <Space size={[4, 4]} wrap className="review-paths">
      {visible.map((page) => (
        <Tooltip key={page} title="复制页面路径">
          <Text code copyable={{ text: page, icon: <CopyOutlined /> }}>
            {page}
          </Text>
        </Tooltip>
      ))}
      {pages.length > visible.length && <Tag>+{pages.length - visible.length}</Tag>}
    </Space>
  );
}

function ReviewDetailDrawer({
  review,
  onClose,
  onUpdateStatus,
  onRunAction,
  actionLoading,
}: {
  review: ReviewItem | null;
  onClose: () => void;
  onUpdateStatus: (review: ReviewItem, nextStatus: "resolved" | "dismissed" | "open") => void;
  onRunAction: (review: ReviewItem, action: "create-page" | "deep-research") => void;
  actionLoading: string;
}) {
  const isOpen = review?.Status === "open";
  return (
    <Drawer
      width={620}
      title="审阅任务详情"
      open={Boolean(review)}
      onClose={onClose}
      extra={
        review && (
          <Space>
            {isOpen ? (
              <>
                {review.Type === "missing-page" && (
                  <Button
                    loading={actionLoading === `${review.ID}:create-page`}
                    onClick={() => onRunAction(review, "create-page")}
                  >
                    生成页面草稿
                  </Button>
                )}
                {canDeepResearch(review) && (
                  <Button
                    loading={actionLoading === `${review.ID}:deep-research`}
                    onClick={() => onRunAction(review, "deep-research")}
                  >
                    启动深度研究
                  </Button>
                )}
                <Button type="primary" onClick={() => onUpdateStatus(review, "resolved")}>
                  标记解决
                </Button>
                <Button danger onClick={() => onUpdateStatus(review, "dismissed")}>
                  忽略
                </Button>
              </>
            ) : (
              <Button icon={<RollbackOutlined />} onClick={() => onUpdateStatus(review, "open")}>
                重新打开
              </Button>
            )}
          </Space>
        )
      }
    >
      {review && (
        <Space direction="vertical" size={16} className="page-stack">
          <Space wrap>
            {statusTag(review.Status)}
            <Tag>{review.Type || "review"}</Tag>
            {review.Severity && <SeverityTag severity={review.Severity} />}
          </Space>
          <Title level={4} className="review-detail-title">
            {review.Title}
          </Title>
          <Paragraph className="answer-block">{review.Description || "暂无详情"}</Paragraph>
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label="任务 ID">
              <Text code copyable>
                {review.ID}
              </Text>
            </Descriptions.Item>
            <Descriptions.Item label="项目 ID">{review.ProjectID || "-"}</Descriptions.Item>
            <Descriptions.Item label="来源">{review.SourcePath || "-"}</Descriptions.Item>
            <Descriptions.Item label="创建时间">{formatDateTime(review.CreatedAt)}</Descriptions.Item>
            <Descriptions.Item label="解决时间">{formatDateTime(review.ResolvedAt)}</Descriptions.Item>
            <Descriptions.Item label="解决动作">{review.ResolvedAction || "-"}</Descriptions.Item>
            <Descriptions.Item label="搜索 Query">
              <ReviewQueries queries={review.SearchQueries ?? []} />
            </Descriptions.Item>
            <Descriptions.Item label="相关页面">
              <ReviewPagePaths pages={review.AffectedPages ?? []} />
            </Descriptions.Item>
            <Descriptions.Item label="可用动作">
              <Space wrap>
                {(review.Options ?? []).map((option) => (
                  <Tag key={option.Action}>{option.Label}</Tag>
                ))}
              </Space>
            </Descriptions.Item>
          </Descriptions>
        </Space>
      )}
    </Drawer>
  );
}

function ReviewQueries({ queries }: { queries: string[] }) {
  if (queries.length === 0) {
    return <Text type="secondary">-</Text>;
  }
  return (
    <Space direction="vertical" size={4}>
      {queries.map((query) => (
        <Text key={query} code copyable>
          {query}
        </Text>
      ))}
    </Space>
  );
}

function WikiOpsPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message, modal } = AntApp.useApp();
  const [sourcePath, setSourcePath] = useState("");
  const [title, setTitle] = useState("");
  const [output, setOutput] = useState<unknown>(null);
  const [loading, setLoading] = useState(false);

  const run = async (label: string, action: () => Promise<unknown>, confirm = false) => {
    const execute = async () => {
      setLoading(true);
      try {
        const result = await action();
        setOutput(result);
        message.success(`${label}完成`);
      } catch (error) {
        message.error(errorMessage(error));
      } finally {
        setLoading(false);
      }
    };
    if (confirm) {
      modal.confirm({ title: label, onOk: execute });
      return;
    }
    await execute();
  };

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="panel">
        <Space wrap>
          <Button loading={loading} onClick={() => run("结构检查", () => api.lint(settings.projectPath))}>
            结构检查
          </Button>
          <Button
            loading={loading}
            onClick={() =>
              run(
                "LLM Wiki 审阅",
                () => api.wikiReview({ project_path: settings.projectPath, agent: settings.agent }),
                true,
              )
            }
          >
            审阅 Wiki
          </Button>
          <Button
            loading={loading}
            onClick={() =>
              run(
                "同步 PostgreSQL",
                () => api.syncWikiPG({ project_path: settings.projectPath, project_id: settings.projectID }),
                true,
              )
            }
          >
            同步 PG
          </Button>
          <Button
            loading={loading}
            onClick={() =>
              run(
                "清理过期审阅任务",
                () =>
                  api.sweepReviews({
                    project_path: settings.projectPath,
                    project_id: settings.projectID,
                    agent: settings.agent,
                  }),
                true,
              )
            }
          >
            清理审阅任务
          </Button>
        </Space>
      </div>
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="来源路径">
            <Input value={sourcePath} onChange={(event) => setSourcePath(event.target.value)} />
          </Form.Item>
          <Form.Item label="标题">
            <Input value={title} onChange={(event) => setTitle(event.target.value)} />
          </Form.Item>
          <Button
            type="primary"
            loading={loading}
            onClick={() =>
              run("验证 Wiki", () =>
                api.validateWiki({
                  project_path: settings.projectPath,
                  project_id: settings.projectID,
                  source_path: sourcePath,
                  title,
                  agent: settings.agent,
                  skip_unchanged: true,
                }),
              )
            }
          >
            验证来源
          </Button>
        </Form>
      </div>
      {output ? <OperationOutput value={output} /> : null}
    </Space>
  );
}

function SettingsPage({
  settings,
  updateSettings,
}: {
  settings: AppSettings;
  updateSettings: (patch: Partial<AppSettings>) => void;
}) {
  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="API 地址">
            <Input
              value={settings.apiBaseUrl}
              onChange={(event) => updateSettings({ apiBaseUrl: event.target.value || "/api" })}
            />
          </Form.Item>
          <Form.Item label="项目路径">
            <Input value={settings.projectPath} readOnly />
          </Form.Item>
          <Form.Item label="项目 ID">
            <Input value={settings.projectID} readOnly />
          </Form.Item>
          <Form.Item label="智能体">
            <Tag color="blue">LLM required</Tag>
          </Form.Item>
        </Form>
      </div>
      <div className="panel">
        <Space direction="vertical" size={12} className="page-stack">
          <Title level={4}>API / MCP</Title>
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label="API Token">
              通过 config.yaml 的 server.api_token 配置；页面不显示明文
            </Descriptions.Item>
            <Descriptions.Item label="MCP 命令">
              <Text code copyable>
                go run ./cmd/kbcore --config config.yaml mcp
              </Text>
            </Descriptions.Item>
          </Descriptions>
        </Space>
      </div>
    </Space>
  );
}

function TaskTable({ tasks }: { tasks: IngestTask[] }) {
  const columns: TableColumnsType<IngestTask> = [
    { title: "来源", dataIndex: "source_path", ellipsis: true },
    { title: "标题", dataIndex: "title", width: 180, ellipsis: true },
    { title: "状态", dataIndex: "status", width: 120, render: (value) => statusTag(value) },
    { title: "重试次数", dataIndex: "retry_count", width: 90 },
    { title: "文件数", dataIndex: "files", render: (files: string[]) => files?.length ?? 0, width: 80 },
    { title: "错误", dataIndex: "error", ellipsis: true },
  ];
  return <Table rowKey="id" columns={columns} dataSource={tasks} />;
}

function SourceManifestTable({
  sources,
  loading,
  onDelete,
}: {
  sources: SourceManifestEntry[];
  loading: boolean;
  onDelete: (source: SourceManifestEntry) => void;
}) {
  const columns: TableColumnsType<SourceManifestEntry> = [
    { title: "标题", dataIndex: "title", width: 180, ellipsis: true },
		{
			title: "来源归档",
			dataIndex: "archive_path",
			ellipsis: true,
			render: (value, source) => value ? <Space direction="vertical" size={0}><Text code>{value}</Text><Text type="secondary">内容：{source.content_path || source.raw_path}</Text></Space> : <Space><Tag color="orange">旧布局</Tag><Text code>{source.raw_path}</Text></Space>,
		},
    { title: "生成页面", dataIndex: "files", render: (files: string[]) => files?.length ?? 0, width: 90 },
    { title: "审阅数", dataIndex: "review_count", width: 90 },
    { title: "更新时间", dataIndex: "updated_at", render: (value) => formatDateTime(value), width: 170 },
    {
      title: "操作",
      width: 110,
      render: (_, source) => (
        <Button size="small" danger loading={loading} onClick={() => onDelete(source)}>
          删除
        </Button>
      ),
    },
  ];
  return (
    <div className="panel">
      <Space direction="vertical" size={12} className="page-stack">
        <Title level={4}>来源清单</Title>
        <Table rowKey="key" columns={columns} dataSource={sources} />
      </Space>
    </div>
  );
}

function TraceTable({ trace }: { trace: QueryAnswer["trace"] }) {
  const columns: TableColumnsType<NonNullable<QueryAnswer["trace"]>[number]> = [
    { title: "步骤", dataIndex: "step", width: 80 },
    { title: "动作", render: (_, item) => item.action.action, width: 130 },
    {
      title: "目标",
      render: (_, item) => item.action.path || item.action.query || item.action.rationale || "-",
      ellipsis: true,
    },
    { title: "观察结果", dataIndex: "observation", ellipsis: true },
  ];
  return <Table rowKey="step" columns={columns} dataSource={trace ?? []} pagination={false} />;
}

function CitationsTable({
  citations,
  onOpenFile,
}: {
  citations: NonNullable<QueryAnswer["citations"]>;
  onOpenFile?: (path: string) => void;
}) {
  const columns: TableColumnsType<NonNullable<QueryAnswer["citations"]>[number]> = [
    {
      title: "路径",
      dataIndex: "path",
      ellipsis: true,
      render: (path: string) =>
        onOpenFile ? (
          <Button type="link" className="path-link" onClick={() => onOpenFile(path)}>
            {path}
          </Button>
        ) : (
          path
        ),
    },
    { title: "标题", dataIndex: "title", ellipsis: true },
    { title: "类型", dataIndex: "kind", width: 150 },
  ];
  return <Table rowKey="path" columns={columns} dataSource={citations} pagination={false} />;
}

function ResultsTable({
  results,
  onOpenFile,
}: {
  results: NonNullable<QueryAnswer["results"]>;
  onOpenFile?: (path: string) => void;
}) {
  const columns: TableColumnsType<NonNullable<QueryAnswer["results"]>[number]> = [
    { title: "标题", dataIndex: "title", ellipsis: true },
    {
      title: "路径",
      dataIndex: "path",
      ellipsis: true,
      render: (path: string) =>
        onOpenFile ? (
          <Button type="link" className="path-link" onClick={() => onOpenFile(path)}>
            {path}
          </Button>
        ) : (
          path
        ),
    },
    { title: "类型", dataIndex: "kind", width: 140 },
    { title: "分数", dataIndex: "score", width: 100 },
    { title: "摘要", dataIndex: "snippet", ellipsis: true },
  ];
  return <Table rowKey="path" columns={columns} dataSource={results} />;
}

function OperationOutput({ value }: { value: unknown }) {
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

function JsonBlock({ value }: { value: unknown }) {
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

function buildFileTree(files: ProjectFile[]): DataNode[] {
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

function buildSourceTree(files: ProjectFile[]): DataNode[] {
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

function rawSourceRelativePath(filePath: string): string {
  return filePath.replace(/^raw\/sources\/?/, "");
}

function sourceTreeKeyForTargetDir(targetDir: string): string {
  const normalized = targetDir.trim().replace(/^\/+|\/+$/g, "");
  return normalized ? `raw/sources/${normalized}` : "raw/sources";
}

function sourceTargetDirFromKey(key: string, rawSourcePathSet: Set<string>): string {
  let dirKey = key || "raw/sources";
  if (rawSourcePathSet.has(dirKey)) {
    const lastSlash = dirKey.lastIndexOf("/");
    dirKey = lastSlash > 0 ? dirKey.slice(0, lastSlash) : "raw/sources";
  }
  if (dirKey === "raw/sources") return "";
  return dirKey.startsWith("raw/sources/") ? dirKey.slice("raw/sources/".length) : "";
}

function uploadRelativePath(file: File): string {
  const withDirectory = file as File & { webkitRelativePath?: string };
  return withDirectory.webkitRelativePath || file.name;
}

function resolveProjectPath(input: string, files: ProjectFile[]): string {
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

function normalizeWikiTarget(input: string): string {
  let value = input.trim().split("|")[0].split("#")[0];
  if (!value) return "";
  if (value.startsWith("raw/") || value === "purpose.md" || value === "schema.md") return value;
  if (value.startsWith("wiki/")) return value.endsWith(".md") ? value : `${value}.md`;
  if (value.includes("/") && value.endsWith(".md")) return value;
  if (value.includes("/") && !value.endsWith(".md")) return `${value}.md`;
  return value;
}

function renderMarkdown(content: string, onOpenWikiLink: (path: string) => void): ReactNode[] {
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

function renderInlineMarkdown(text: string, onOpenWikiLink: (path: string) => void): ReactNode[] {
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

function upsertServerSession(sessions: ChatSessionRecord[], next: ChatSessionRecord): ChatSessionRecord[] {
  const filtered = sessions.filter((session) => session.id !== next.id);
  return [next, ...filtered].sort((a, b) => b.updated_at.localeCompare(a.updated_at));
}

function isScanSourcesResult(value: unknown): value is ScanSourcesResult {
  return Boolean(value && typeof value === "object" && "queued" in value && "skipped" in value);
}

function truncateLabel(value: string, max: number): string {
  return value.length > max ? `${value.slice(0, max - 1)}...` : value;
}

function slugify(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/\.md$/i, "")
    .replace(/[_\s]+/g, "-");
}

function statusTag(status: string) {
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

function statusLabel(status: string): string {
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

function maintainStepLabel(step: string): string {
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

function summaryText(summary?: Record<string, unknown>): string {
  if (!summary) {
    return "-";
  }
  return Object.entries(summary)
    .map(([key, value]) => `${key}=${String(value)}`)
    .join(" ");
}

function canDeepResearch(review: ReviewItem): boolean {
  return ["missing-page", "source-gap", "review-needed", "suggestion"].includes(review.Type);
}

function SeverityTag({ severity }: { severity: string }) {
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

function formatDateTime(value?: string | null): string {
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

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
