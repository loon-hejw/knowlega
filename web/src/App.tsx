import {
  ApiOutlined,
  CheckCircleOutlined,
  ClusterOutlined,
  DatabaseOutlined,
  FileSearchOutlined,
  InboxOutlined,
  MessageOutlined,
  PlayCircleOutlined,
  ReloadOutlined,
  SettingOutlined,
  SyncOutlined,
  WarningOutlined,
} from "@ant-design/icons";
import {
  Alert,
  App as AntApp,
  Button,
  Collapse,
  Descriptions,
  Form,
  Input,
  InputNumber,
  Layout,
  Menu,
  Modal,
  Select,
  Space,
  Statistic,
  Switch,
  Table,
  Tabs,
  Tag,
  Typography,
} from "antd";
import type { MenuProps, TableColumnsType } from "antd";
import { useCallback, useEffect, useMemo, useState } from "react";
import { ApiClient, isQueryWritebackResponse } from "./api/client";
import type {
  AgentName,
  AppSettings,
  HealthResponse,
  IngestTask,
  IssuesResponse,
  QueryAnswer,
  QueryResponseWithWriteback,
  ReviewItem,
  ReviewsResponse,
  RunQueueResult,
  ScanSourcesResult,
  ValidateWikiResult,
} from "./types/api";

const { Header, Sider, Content } = Layout;
const { Text, Title, Paragraph } = Typography;

const defaultSettings: AppSettings = {
  apiBaseUrl: "/api",
  projectPath: "",
  projectID: "",
  agent: "auto",
};

type PageKey = "dashboard" | "query" | "sources" | "reviews" | "wikiops" | "settings";

function loadSettings(): AppSettings {
  try {
    const raw = localStorage.getItem("knowledge-core.settings");
    return raw ? { ...defaultSettings, ...JSON.parse(raw) } : defaultSettings;
  } catch {
    return defaultSettings;
  }
}

function saveSettings(settings: AppSettings): void {
  localStorage.setItem("knowledge-core.settings", JSON.stringify(settings));
}

export default function App() {
  const [settings, setSettings] = useState<AppSettings>(() => loadSettings());
  const [page, setPage] = useState<PageKey>("dashboard");
  const api = useMemo(() => new ApiClient(settings.apiBaseUrl), [settings.apiBaseUrl]);

  const updateSettings = (patch: Partial<AppSettings>) => {
    setSettings((current) => {
      const next = { ...current, ...patch };
      saveSettings(next);
      return next;
    });
  };

  const menuItems: MenuProps["items"] = [
    { key: "dashboard", icon: <ClusterOutlined />, label: "Dashboard" },
    { key: "query", icon: <MessageOutlined />, label: "Query" },
    { key: "sources", icon: <InboxOutlined />, label: "Sources" },
    { key: "reviews", icon: <WarningOutlined />, label: "Reviews" },
    { key: "wikiops", icon: <FileSearchOutlined />, label: "Wiki Ops" },
    { key: "settings", icon: <SettingOutlined />, label: "Settings" },
  ];

  return (
    <AntApp>
      <Layout className="app-shell">
        <Sider width={248} className="app-sider">
          <div className="brand">
            <DatabaseOutlined />
            <span>Knowledge Core</span>
          </div>
          <Menu
            mode="inline"
            selectedKeys={[page]}
            items={menuItems}
            onClick={({ key }) => setPage(key as PageKey)}
          />
        </Sider>
        <Layout>
          <Header className="app-header">
            <div className="header-title">
              <Title level={3}>{titleForPage(page)}</Title>
              <Text type="secondary">{settings.projectPath || "No project selected"}</Text>
            </div>
            <Space>
              <Tag icon={<ApiOutlined />} color={settings.apiBaseUrl === "/api" ? "blue" : "geekblue"}>
                {settings.apiBaseUrl}
              </Tag>
              <Tag>{settings.agent}</Tag>
            </Space>
          </Header>
          <Content className="app-content">
            {page === "dashboard" && <Dashboard api={api} settings={settings} />}
            {page === "query" && <QueryPage api={api} settings={settings} />}
            {page === "sources" && <SourcesPage api={api} settings={settings} />}
            {page === "reviews" && <ReviewsPage api={api} settings={settings} />}
            {page === "wikiops" && <WikiOpsPage api={api} settings={settings} />}
            {page === "settings" && (
              <SettingsPage settings={settings} updateSettings={updateSettings} api={api} />
            )}
          </Content>
        </Layout>
      </Layout>
    </AntApp>
  );
}

function titleForPage(page: PageKey): string {
  switch (page) {
    case "dashboard":
      return "Dashboard";
    case "query":
      return "Query";
    case "sources":
      return "Sources";
    case "reviews":
      return "Reviews";
    case "wikiops":
      return "Wiki Ops";
    case "settings":
      return "Settings";
  }
}

function Dashboard({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message } = AntApp.useApp();
  const [health, setHealth] = useState<HealthResponse | null>(null);
  const [queue, setQueue] = useState<IngestTask[]>([]);
  const [reviews, setReviews] = useState<ReviewItem[]>([]);
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [healthData, queueData, reviewData] = await Promise.all([
        api.health(),
        api.queueTasks(settings.projectPath),
        api.reviews(settings.projectPath, settings.projectID, "open"),
      ]);
      setHealth(healthData);
      setQueue(queueData.tasks ?? []);
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

  const pending = queue.filter((task) => task.status === "pending").length;
  const failed = queue.filter((task) => task.status === "failed").length;

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="toolbar">
        <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>
          Refresh
        </Button>
      </div>
      <div className="metric-grid">
        <div className="metric-panel">
          <Statistic title="Service" value={health?.ok ? "Healthy" : "Unknown"} />
          <Text type="secondary">{health?.time ?? "No response yet"}</Text>
        </div>
        <div className="metric-panel">
          <Statistic title="Pending Queue" value={pending} />
          <Text type={failed > 0 ? "danger" : "secondary"}>{failed} failed</Text>
        </div>
        <div className="metric-panel">
          <Statistic title="Open Reviews" value={reviews.length} />
          <Text type="secondary">{settings.agent} agent</Text>
        </div>
      </div>
      <Descriptions bordered size="small" column={1}>
        <Descriptions.Item label="Project Path">{settings.projectPath || "-"}</Descriptions.Item>
        <Descriptions.Item label="Project ID">{settings.projectID || "-"}</Descriptions.Item>
        <Descriptions.Item label="API Base">{settings.apiBaseUrl}</Descriptions.Item>
      </Descriptions>
    </Space>
  );
}

function QueryPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message, modal } = AntApp.useApp();
  const [question, setQuestion] = useState("");
  const [agent, setAgent] = useState<AgentName>(settings.agent);
  const [limit, setLimit] = useState<number>(10);
  const [saveTitle, setSaveTitle] = useState("");
  const [loading, setLoading] = useState(false);
  const [response, setResponse] = useState<QueryAnswer | QueryResponseWithWriteback | null>(null);

  const answer = response && isQueryWritebackResponse(response) ? response.answer : response;

  const submit = async () => {
    if (!question.trim()) {
      message.warning("Question is required");
      return;
    }
    const run = async () => {
      setLoading(true);
      try {
        const data = await api.query({
          project_path: settings.projectPath,
          project_id: settings.projectID,
          q: question,
          limit,
          agent,
          save_title: saveTitle.trim() || undefined,
        });
        setResponse(data);
        message.success(saveTitle.trim() ? "Query saved" : "Query complete");
      } catch (error) {
        message.error(errorMessage(error));
      } finally {
        setLoading(false);
      }
    };
    if (saveTitle.trim()) {
      modal.confirm({
        title: "Save synthesis",
        content: saveTitle.trim(),
        icon: <CheckCircleOutlined />,
        onOk: run,
      });
      return;
    }
    await run();
  };

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="Question">
            <Input.TextArea
              rows={4}
              value={question}
              onChange={(event) => setQuestion(event.target.value)}
            />
          </Form.Item>
          <div className="form-row">
            <Form.Item label="Agent">
              <AgentSelect value={agent} onChange={setAgent} includeFallback />
            </Form.Item>
            <Form.Item label="Limit">
              <InputNumber min={1} max={50} value={limit} onChange={(value) => setLimit(value ?? 10)} />
            </Form.Item>
            <Form.Item label="Save Title">
              <Input
                value={saveTitle}
                onChange={(event) => setSaveTitle(event.target.value)}
                placeholder="Optional"
              />
            </Form.Item>
          </div>
          <Button type="primary" icon={<PlayCircleOutlined />} loading={loading} onClick={submit}>
            Run Query
          </Button>
        </Form>
      </div>
      {answer && (
        <div className="panel">
          <Space direction="vertical" size={14} className="page-stack">
            <Space wrap>
              <Tag color="blue">{answer.plan.intent}</Tag>
              <Tag color="geekblue">{answer.plan.answer_mode}</Tag>
              <Tag color={answer.plan.can_write_back ? "green" : "default"}>
                writeback {String(answer.plan.can_write_back)}
              </Tag>
              {isQueryWritebackResponse(response!) && <Tag color="green">{response.writeback.path}</Tag>}
            </Space>
            <Paragraph className="answer-block">{answer.answer}</Paragraph>
            <Tabs
              items={[
                {
                  key: "trace",
                  label: "Trace",
                  children: <TraceTable trace={answer.trace ?? []} />,
                },
                {
                  key: "citations",
                  label: "Citations",
                  children: <CitationsTable citations={answer.citations ?? []} />,
                },
                {
                  key: "results",
                  label: "Results",
                  children: <ResultsTable results={answer.results ?? []} />,
                },
                {
                  key: "plan",
                  label: "Plan",
                  children: <JsonBlock value={answer.plan} />,
                },
              ]}
            />
            {(answer.notes ?? []).length > 0 && (
              <Alert type="info" showIcon message={(answer.notes ?? []).join(" ")} />
            )}
          </Space>
        </div>
      )}
    </Space>
  );
}

function SourcesPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message, modal } = AntApp.useApp();
  const [sourcePath, setSourcePath] = useState("");
  const [title, setTitle] = useState("");
  const [queue, setQueue] = useState<IngestTask[]>([]);
  const [lastRun, setLastRun] = useState<RunQueueResult | ScanSourcesResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [keepDone, setKeepDone] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const data = await api.queueTasks(settings.projectPath);
      setQueue(data.tasks ?? []);
    } catch (error) {
      message.error(errorMessage(error));
    }
  }, [api, message, settings.projectPath]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const queueSource = async () => {
    if (!sourcePath.trim()) {
      message.warning("Source path is required");
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
      message.success("Queued");
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  const scanSources = async () => {
    setLoading(true);
    try {
      const data = await api.scanSources(settings.projectPath);
      setLastRun(data);
      await refresh();
      message.success("Scan complete");
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };

  const runQueue = () => {
    modal.confirm({
      title: "Run ingest queue",
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
          message.success("Queue complete");
        } catch (error) {
          message.error(errorMessage(error));
        } finally {
          setLoading(false);
        }
      },
    });
  };

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="Source Path">
            <Input value={sourcePath} onChange={(event) => setSourcePath(event.target.value)} />
          </Form.Item>
          <Form.Item label="Title">
            <Input value={title} onChange={(event) => setTitle(event.target.value)} />
          </Form.Item>
          <Space wrap>
            <Button type="primary" loading={loading} onClick={queueSource}>
              Queue Source
            </Button>
            <Button icon={<ReloadOutlined />} loading={loading} onClick={scanSources}>
              Scan Sources
            </Button>
            <Button icon={<PlayCircleOutlined />} loading={loading} onClick={runQueue}>
              Run Queue
            </Button>
            <Switch checked={keepDone} onChange={setKeepDone} />
            <Text>Keep done</Text>
          </Space>
        </Form>
      </div>
      <TaskTable tasks={queue} />
      {lastRun && <JsonBlock value={lastRun} />}
    </Space>
  );
}

function ReviewsPage({ api, settings }: { api: ApiClient; settings: AppSettings }) {
  const { message, modal } = AntApp.useApp();
  const [status, setStatus] = useState("open");
  const [data, setData] = useState<ReviewsResponse | null>(null);
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setData(await api.reviews(settings.projectPath, settings.projectID, status));
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setLoading(false);
    }
  }, [api, message, settings.projectID, settings.projectPath, status]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const updateStatus = (review: ReviewItem, nextStatus: "resolved" | "dismissed" | "open") => {
    modal.confirm({
      title: nextStatus === "open" ? "Reopen review" : `${nextStatus} review`,
      content: review.Title,
      onOk: async () => {
        try {
          await api.resolveReview({
            project_path: settings.projectPath,
            project_id: settings.projectID,
            id: review.ID,
            status: nextStatus,
          });
          await refresh();
          message.success("Review updated");
        } catch (error) {
          message.error(errorMessage(error));
        }
      },
    });
  };

  const columns: TableColumnsType<ReviewItem> = [
    { title: "Title", dataIndex: "Title", ellipsis: true },
    { title: "Type", dataIndex: "Type", width: 150 },
    { title: "Status", dataIndex: "Status", width: 120, render: (value) => <Tag>{value}</Tag> },
    {
      title: "Affected Pages",
      dataIndex: "AffectedPages",
      render: (pages: string[]) => pages?.slice(0, 3).join(", "),
    },
    {
      title: "Actions",
      width: 210,
      render: (_, review) => (
        <Space>
          <Button size="small" onClick={() => updateStatus(review, "resolved")}>
            Resolve
          </Button>
          <Button size="small" onClick={() => updateStatus(review, "dismissed")}>
            Dismiss
          </Button>
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="toolbar">
        <Select
          value={status}
          onChange={setStatus}
          options={[
            { value: "open", label: "Open" },
            { value: "resolved", label: "Resolved" },
            { value: "dismissed", label: "Dismissed" },
          ]}
        />
        <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>
          Refresh
        </Button>
      </div>
      <Table rowKey="ID" columns={columns} dataSource={data?.reviews ?? []} loading={loading} />
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
        message.success(`${label} complete`);
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
          <Button loading={loading} onClick={() => run("Lint", () => api.lint(settings.projectPath))}>
            Lint
          </Button>
          <Button
            loading={loading}
            onClick={() =>
              run(
                "LLM Wiki Review",
                () => api.wikiReview({ project_path: settings.projectPath, agent: settings.agent }),
                true,
              )
            }
          >
            Review Wiki
          </Button>
          <Button
            loading={loading}
            onClick={() =>
              run(
                "Sync PostgreSQL",
                () => api.syncWikiPG({ project_path: settings.projectPath, project_id: settings.projectID }),
                true,
              )
            }
          >
            Sync PG
          </Button>
        </Space>
      </div>
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="Source Path">
            <Input value={sourcePath} onChange={(event) => setSourcePath(event.target.value)} />
          </Form.Item>
          <Form.Item label="Title">
            <Input value={title} onChange={(event) => setTitle(event.target.value)} />
          </Form.Item>
          <Button
            type="primary"
            loading={loading}
            onClick={() =>
              run("Validate Wiki", () =>
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
            Validate Source
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
  api,
}: {
  settings: AppSettings;
  updateSettings: (patch: Partial<AppSettings>) => void;
  api: ApiClient;
}) {
  const { message } = AntApp.useApp();
  const [projectName, setProjectName] = useState("");

  const initProject = async () => {
    if (!settings.projectPath.trim()) {
      message.warning("Project path is required");
      return;
    }
    try {
      await api.initProject(settings.projectPath, projectName || "knowledge-core");
      message.success("Project initialized");
    } catch (error) {
      message.error(errorMessage(error));
    }
  };

  return (
    <Space direction="vertical" size={16} className="page-stack">
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="API Base URL">
            <Input
              value={settings.apiBaseUrl}
              onChange={(event) => updateSettings({ apiBaseUrl: event.target.value || "/api" })}
            />
          </Form.Item>
          <Form.Item label="Project Path">
            <Input
              value={settings.projectPath}
              onChange={(event) => updateSettings({ projectPath: event.target.value })}
            />
          </Form.Item>
          <Form.Item label="Project ID">
            <Input
              value={settings.projectID}
              onChange={(event) => updateSettings({ projectID: event.target.value })}
            />
          </Form.Item>
          <Form.Item label="Agent">
            <AgentSelect value={settings.agent} onChange={(agent) => updateSettings({ agent })} />
          </Form.Item>
        </Form>
      </div>
      <div className="panel">
        <Form layout="vertical">
          <Form.Item label="Project Name">
            <Input value={projectName} onChange={(event) => setProjectName(event.target.value)} />
          </Form.Item>
          <Button onClick={initProject}>Initialize Project</Button>
        </Form>
      </div>
    </Space>
  );
}

function AgentSelect({
  value,
  onChange,
  includeFallback = false,
}: {
  value: AgentName;
  onChange: (value: AgentName) => void;
  includeFallback?: boolean;
}) {
  const options: Array<{ value: AgentName; label: string }> = [
    { value: "auto", label: "auto" },
    { value: "mock", label: "mock" },
    { value: "llm", label: "llm" },
  ];
  if (includeFallback) options.push({ value: "fallback", label: "fallback" });
  return <Select value={value} onChange={onChange} options={options} className="control-wide" />;
}

function TaskTable({ tasks }: { tasks: IngestTask[] }) {
  const columns: TableColumnsType<IngestTask> = [
    { title: "Source", dataIndex: "source_path", ellipsis: true },
    { title: "Title", dataIndex: "title", width: 180, ellipsis: true },
    { title: "Status", dataIndex: "status", width: 120, render: (value) => statusTag(value) },
    { title: "Retries", dataIndex: "retry_count", width: 90 },
    { title: "Files", dataIndex: "files", render: (files: string[]) => files?.length ?? 0, width: 80 },
    { title: "Error", dataIndex: "error", ellipsis: true },
  ];
  return <Table rowKey="id" columns={columns} dataSource={tasks} />;
}

function TraceTable({ trace }: { trace: QueryAnswer["trace"] }) {
  const columns: TableColumnsType<NonNullable<QueryAnswer["trace"]>[number]> = [
    { title: "Step", dataIndex: "step", width: 80 },
    { title: "Action", render: (_, item) => item.action.action, width: 130 },
    {
      title: "Target",
      render: (_, item) => item.action.path || item.action.query || item.action.rationale || "-",
      ellipsis: true,
    },
    { title: "Observation", dataIndex: "observation", ellipsis: true },
  ];
  return <Table rowKey="step" columns={columns} dataSource={trace ?? []} pagination={false} />;
}

function CitationsTable({ citations }: { citations: NonNullable<QueryAnswer["citations"]> }) {
  const columns: TableColumnsType<NonNullable<QueryAnswer["citations"]>[number]> = [
    { title: "Path", dataIndex: "path", ellipsis: true },
    { title: "Title", dataIndex: "title", ellipsis: true },
    { title: "Kind", dataIndex: "kind", width: 150 },
  ];
  return <Table rowKey="path" columns={columns} dataSource={citations} pagination={false} />;
}

function ResultsTable({ results }: { results: NonNullable<QueryAnswer["results"]> }) {
  const columns: TableColumnsType<NonNullable<QueryAnswer["results"]>[number]> = [
    { title: "Title", dataIndex: "title", ellipsis: true },
    { title: "Path", dataIndex: "path", ellipsis: true },
    { title: "Kind", dataIndex: "kind", width: 140 },
    { title: "Score", dataIndex: "score", width: 100 },
    { title: "Snippet", dataIndex: "snippet", ellipsis: true },
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
            { title: "Type", dataIndex: "Type", width: 160 },
            { title: "Path", dataIndex: "Path", width: 220, ellipsis: true },
            { title: "Detail", dataIndex: "Detail" },
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

function statusTag(status: string) {
  const color =
    status === "done" || status === "resolved"
      ? "green"
      : status === "failed" || status === "dismissed"
        ? "red"
        : status === "processing"
          ? "blue"
          : "gold";
  return <Tag color={color}>{status}</Tag>;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
