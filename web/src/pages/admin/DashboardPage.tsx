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


export function Dashboard({ api, settings }: { api: ApiClient; settings: AppSettings }) {
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

export function MaintainJobPanel({ job }: { job: WorkspaceJob }) {
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

export function MaintainResultPanel({ result }: { result: MaintainWikiResult }) {
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
