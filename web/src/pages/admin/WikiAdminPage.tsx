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


import { WorkbenchPage } from "../../features/wiki/WorkbenchPage";
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


export function WikiOpsPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
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

  const maintenancePanel = (
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
  return (
    <Tabs
      className="admin-wiki-tabs"
      items={[
        { key: "maintenance", label: "维护操作", children: maintenancePanel },
        { key: "content", label: "内容编辑", children: <WorkbenchPage api={api} settings={settings} initialPath="wiki/index.md" /> },
      ]}
    />
  );
}

export function SettingsPage({
  settings,
  updateSettings,
}: {
  settings: AppSettings;
  updateSettings: (patch: Partial<AppSettings>) => void;
}) {
  return (
    <Space direction="vertical" size={16} className="page-stack">
      <ProCard title="运行配置" bordered>
        <ProForm submitter={false} layout="vertical">
          <ProFormText
            name="apiBaseUrl"
            label="API 地址"
            fieldProps={{
              value: settings.apiBaseUrl,
              onChange: (event) => updateSettings({ apiBaseUrl: event.target.value || "/api" }),
            }}
          />
          <ProFormText name="projectPath" label="项目路径" fieldProps={{ value: settings.projectPath, readOnly: true }} />
          <ProFormText name="projectID" label="项目 ID" fieldProps={{ value: settings.projectID, readOnly: true }} />
          <Form.Item label="智能体"><Tag color="blue">LLM required</Tag></Form.Item>
        </ProForm>
      </ProCard>
      <ProCard title="API / MCP" bordered>
        <Space direction="vertical" size={12} className="page-stack">
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
      </ProCard>
    </Space>
  );
}

export function TaskTable({ tasks }: { tasks: IngestTask[] }) {
  const columns: ProColumns<IngestTask>[] = [
    { title: "来源", dataIndex: "source_path", ellipsis: true },
    { title: "标题", dataIndex: "title", width: 180, ellipsis: true },
    { title: "状态", dataIndex: "status", width: 120, render: (_, task) => statusTag(task.status) },
    { title: "重试次数", dataIndex: "retry_count", width: 90 },
    { title: "文件数", dataIndex: "files", render: (_, task) => task.files?.length ?? 0, width: 80 },
    { title: "错误", dataIndex: "error", ellipsis: true },
  ];
  return <ProTable rowKey="id" columns={columns} dataSource={tasks} search={false} options={false} toolBarRender={false} />;
}

export function SourceManifestTable({
  sources,
  loading,
  onDelete,
}: {
  sources: SourceManifestEntry[];
  loading: boolean;
  onDelete: (source: SourceManifestEntry) => void;
}) {
  const columns: ProColumns<SourceManifestEntry>[] = [
    { title: "标题", dataIndex: "title", width: 180, ellipsis: true },
		{
			title: "来源归档",
			dataIndex: "archive_path",
			ellipsis: true,
			render: (_, source) => source.archive_path ? <Space direction="vertical" size={0}><Text code>{source.archive_path}</Text><Text type="secondary">内容：{source.content_path || source.raw_path}</Text></Space> : <Space><Tag color="orange">旧布局</Tag><Text code>{source.raw_path}</Text></Space>,
		},
    { title: "生成页面", dataIndex: "files", render: (_, source) => source.files?.length ?? 0, width: 90 },
    { title: "审阅数", dataIndex: "review_count", width: 90 },
    { title: "更新时间", dataIndex: "updated_at", render: (_, source) => formatDateTime(source.updated_at), width: 170 },
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
        <ProTable
          rowKey="key"
          columns={columns}
          dataSource={sources}
          search={false}
          options={false}
          toolBarRender={false}
        />
      </Space>
    </div>
  );
}
