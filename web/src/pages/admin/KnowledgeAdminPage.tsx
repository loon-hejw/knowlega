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
import { ApiClient } from "../../api/client";
import { summarizeWikiPages } from "../../features/wiki/WorkbenchPage";
import { MaintainJobPanel } from "./DashboardPage";
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


export function KnowledgePage({
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
