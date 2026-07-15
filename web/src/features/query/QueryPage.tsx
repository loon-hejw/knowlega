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


import {
  ChatBubble,
  ChatRunBubble,
  isAbortError,
  terminalChatRunStatus,
  upsertRunEvent,
} from "./QueryRun";
import type { ActiveChatRunState } from "./QueryRun";
import { CitationsTable, ResultsTable, TraceTable } from "./QueryTables";
export function QueryPage({
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
