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
import { ProCard, ProForm, ProFormText, ProTable } from "@ant-design/pro-components";
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


import { SourceManifestTable, TaskTable } from "./WikiAdminPage";
export function SourcesPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
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
