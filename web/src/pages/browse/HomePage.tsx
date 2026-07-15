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


import homeStyles from "./HomePage.module.css";
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


export type HomeActivityItem = {
  key: string;
  title: string;
  subtitle: string;
  updatedAt: string;
  kind: "wiki" | "code" | "research" | "collection";
  path?: string;
};

export type KnowledgeMapGroup = {
  key: string;
  label: string;
  description: string;
  query: string;
  nodes: number;
  evidence: number;
  domain: string;
};

export function BrowserHomePage({
  api,
  settings,
  onNavigate,
  onOpenFile,
  onAsk,
  onTopic,
}: {
  api: ApiClient;
  settings: AppSettings;
  onNavigate: (page: BrowserPageKey) => void;
  onOpenFile: (path: string) => void;
  onAsk: (question: string) => void;
  onTopic: (query: string) => void;
}) {
  const { message } = AntApp.useApp();
  const [files, setFiles] = useState<ProjectFile[]>([]);
  const [sessions, setSessions] = useState<ChatSessionRecord[]>([]);
  const [graph, setGraph] = useState<WikiGraphResponse | null>(null);
  const [insights, setInsights] = useState<WikiGraphInsightsResponse | null>(null);
  const [question, setQuestion] = useState("");
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    const [filesResult, sessionsResult, graphResult, insightsResult] = await Promise.allSettled([
      api.projectFiles(settings.projectPath),
      api.chats(settings.projectPath),
      api.projectGraph({ project_path: settings.projectPath, domains: ["wiki", "source", "code"], limit: 500 }),
      api.wikiGraphInsights(settings.projectPath),
    ]);
    if (filesResult.status === "fulfilled") setFiles(filesResult.value.files ?? []);
    if (sessionsResult.status === "fulfilled") setSessions(sessionsResult.value.sessions ?? []);
    if (graphResult.status === "fulfilled") setGraph(graphResult.value);
    if (insightsResult.status === "fulfilled") setInsights(insightsResult.value);
    if ([filesResult, graphResult].some((result) => result.status === "rejected")) {
      message.warning("部分知识入口暂时不可用");
    }
    setLoading(false);
  }, [api, message, settings.projectPath]);

  useEffect(() => { void refresh(); }, [refresh]);

  const recentQuestions = useMemo(() => {
    const history = sessions.map((session) => session.title?.trim()).filter((title): title is string => Boolean(title));
    return (history.length > 0 ? history : insights?.suggested_questions ?? []).slice(0, 3);
  }, [insights?.suggested_questions, sessions]);

  const activities = useMemo<HomeActivityItem[]>(() => {
    const wikiItems = files
      .filter((file) => file.path.startsWith("wiki/") && file.path.endsWith(".md"))
      .map((file): HomeActivityItem => ({
        key: file.path,
        title: knowledgeTitle(file.path),
        subtitle: homeActivitySubtitle(file.path),
        updatedAt: file.mod_time,
        kind: homeActivityKind(file.path),
        path: file.path,
      }));
    const researchItems = sessions.map((session): HomeActivityItem => ({
      key: `chat:${session.id}`,
      title: session.title || "未命名研究",
      subtitle: "研究记录 · 可继续追问",
      updatedAt: session.updated_at || session.created_at,
      kind: "research",
    }));
    return [...wikiItems, ...researchItems]
      .sort((a, b) => (b.updatedAt || "").localeCompare(a.updatedAt || ""))
      .slice(0, 6);
  }, [files, sessions]);

  const knowledgeGroups = useMemo(() => buildKnowledgeMapGroups(graph?.nodes ?? []), [graph?.nodes]);
  const submit = () => { if (question.trim()) onAsk(question); };

  return (
    <main className={homeStyles.home}>
      <ProCard className={homeStyles.hero} bordered>
        <Title level={1}>从知识中找到答案</Title>
        <Input.Search
          className={homeStyles.search}
          size="large"
          prefix={<SearchOutlined />}
          value={question}
          onChange={(event) => setQuestion(event.target.value)}
          onSearch={(value) => value.trim() && onAsk(value)}
          enterButton={<><SendOutlined /> 提问</>}
          placeholder="搜索或提问，覆盖 Wiki、来源、系统与代码"
          aria-label="搜索或提问"
        />
        {recentQuestions.length > 0 && (
          <Space wrap className={homeStyles.questions} aria-label="最近提问">
            {recentQuestions.map((item) => (
              <Button key={item} size="small" icon={<MessageOutlined />} onClick={() => onAsk(item)}>
                {item}
              </Button>
            ))}
          </Space>
        )}
      </ProCard>

      <Row gutter={[24, 24]} aria-busy={loading}>
        <Col xs={24} xl={13}>
          <ProCard
            className={homeStyles.panel}
            title={<Space><NodeIndexOutlined />继续探索</Space>}
            extra={<Button type="link" onClick={() => onNavigate("wiki")}>查看全部历史 <RightOutlined /></Button>}
            bordered
          >
            <List
              loading={loading}
              dataSource={activities}
              locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无可继续探索的内容" /> }}
              renderItem={(item) => (
                <List.Item
                  key={item.key}
                  actions={[
                    <Text key="time" type="secondary" className={homeStyles.itemMeta}>{formatHomeDate(item.updatedAt)}</Text>,
                    <Button key="open" type="text" icon={<RightOutlined />} aria-label={`打开 ${item.title}`} onClick={() => item.path ? onOpenFile(item.path) : onNavigate("query")} />,
                  ]}
                >
                  <List.Item.Meta
                    avatar={<Avatar className={`${homeStyles.activityIcon} ${homeStyles[item.kind] ?? ""}`} icon={homeActivityIcon(item.kind)} />}
                    title={<Button type="link" className={homeStyles.itemTitle} onClick={() => item.path ? onOpenFile(item.path) : onNavigate("query")}>{item.title}</Button>}
                    description={item.subtitle}
                  />
                </List.Item>
              )}
            />
          </ProCard>
        </Col>
        <Col xs={24} xl={11}>
          <ProCard
            className={homeStyles.panel}
            title={<Space><ClusterOutlined />知识地图</Space>}
            extra={<Button type="link" onClick={() => onNavigate("topics")}>查看所有主题 <RightOutlined /></Button>}
            bordered
          >
            <List
              loading={loading}
              dataSource={knowledgeGroups}
              locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="知识地图将在来源处理后自动形成" /> }}
              renderItem={(group) => (
                <List.Item
                  key={group.key}
                  actions={[
                    <Space key="count" direction="vertical" size={0} className={homeStyles.itemMeta}>
                      <Text strong>{group.nodes} 项知识</Text>
                      <Text type="secondary">{group.evidence} 条证据</Text>
                    </Space>,
                    <Button key="open" type="text" icon={<RightOutlined />} aria-label={`查看 ${group.label}`} onClick={() => onTopic(group.query)} />,
                  ]}
                >
                  <List.Item.Meta
                    avatar={<Avatar className={homeStyles.activityIcon} icon={knowledgeGroupIcon(group.domain)} />}
                    title={<Button type="link" className={homeStyles.itemTitle} onClick={() => onTopic(group.query)}>{group.label}</Button>}
                    description={group.description}
                  />
                </List.Item>
              )}
            />
          </ProCard>
        </Col>
      </Row>
    </main>
  );
}


export function homeActivityKind(path: string): HomeActivityItem["kind"] {
  if (path.startsWith("wiki/code/")) return "code";
  if (path.startsWith("wiki/syntheses/")) return "research";
  if (path.startsWith("wiki/collections/")) return "collection";
  return "wiki";
}

export function homeActivitySubtitle(path: string): string {
  if (path.startsWith("wiki/code/")) return `代码证据 · ${path.replace(/^wiki\/code\//, "").replace(/\.md$/i, "")}`;
  if (path.startsWith("wiki/syntheses/")) return `综合结论 · ${path.replace(/^wiki\/syntheses\//, "").replace(/\.md$/i, "")}`;
  if (path.startsWith("wiki/concepts/")) return `概念 · ${path.replace(/^wiki\/concepts\//, "").replace(/\.md$/i, "")}`;
  if (path.startsWith("wiki/entities/")) return `实体 · ${path.replace(/^wiki\/entities\//, "").replace(/\.md$/i, "")}`;
  if (path.startsWith("wiki/sources/")) return `来源摘要 · ${path.replace(/^wiki\/sources\//, "").replace(/\.md$/i, "")}`;
  return `Wiki · ${path.replace(/^wiki\//, "").replace(/\.md$/i, "")}`;
}

export function homeActivityIcon(kind: HomeActivityItem["kind"]): ReactNode {
  if (kind === "code") return <CodeOutlined />;
  if (kind === "research") return <SearchOutlined />;
  if (kind === "collection") return <BookOutlined />;
  return <FileMarkdownOutlined />;
}

export function buildKnowledgeMapGroups(nodes: WikiGraphResponse["nodes"]): KnowledgeMapGroup[] {
  const grouped = new Map<string, WikiGraphResponse["nodes"]>();
  for (const node of nodes) {
    const rawKey = node.community?.trim() || node.type?.trim() || node.kind?.trim() || "其他知识";
    grouped.set(rawKey, [...(grouped.get(rawKey) ?? []), node]);
  }
  return Array.from(grouped.entries())
    .map(([key, groupNodes]) => {
      const domainCounts = new Map<string, number>();
      const evidence = new Set<string>();
      for (const node of groupNodes) {
        domainCounts.set(node.domain || "wiki", (domainCounts.get(node.domain || "wiki") ?? 0) + 1);
        node.sources?.forEach((source) => evidence.add(source));
        if (node.domain === "source" && (node.path || node.source_ref)) evidence.add(node.path || node.source_ref || node.id);
      }
      const domain = Array.from(domainCounts.entries()).sort((a, b) => b[1] - a[1])[0]?.[0] || "wiki";
      const label = knowledgeGroupLabel(key);
      return {
        key,
        label,
        query: key,
        nodes: groupNodes.length,
        evidence: evidence.size,
        domain,
        description: knowledgeGroupDescription(domain, groupNodes),
      };
    })
    .sort((a, b) => b.nodes - a.nodes || a.label.localeCompare(b.label, "zh-CN"))
    .slice(0, 6);
}

export function knowledgeGroupLabel(value: string): string {
  const labels: Record<string, string> = {
    concept: "概念与方法",
    entity: "实体与对象",
    synthesis: "综合结论",
    source: "来源与资料",
    code: "代码与工程",
    file: "文件与模块",
    function: "函数与调用",
    service: "服务与系统",
  };
  const normalized = value.trim().toLowerCase();
  if (labels[normalized]) return labels[normalized];
  if (/^community[-_: ]?/i.test(value)) return `知识社区 ${value.replace(/^community[-_: ]?/i, "") || ""}`.trim();
  return value.replace(/[-_]+/g, " ");
}

export function knowledgeGroupDescription(domain: string, nodes: WikiGraphResponse["nodes"]): string {
  const kinds = Array.from(new Set(nodes.map((node) => node.kind || node.type).filter(Boolean))).slice(0, 3);
  const prefix = domain === "code" ? "代码证据" : domain === "source" ? "来源材料" : "Wiki 知识";
  return kinds.length > 0 ? `${prefix} · ${kinds.join("、")}` : `${prefix}与关联内容`;
}

export function knowledgeGroupIcon(domain: string): ReactNode {
  if (domain === "code") return <CodeOutlined />;
  if (domain === "source") return <DatabaseOutlined />;
  return <ClusterOutlined />;
}

export function formatHomeDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  const now = new Date();
  if (date.toDateString() === now.toDateString()) return `今天 ${formatShortTime(value)}`;
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  if (date.toDateString() === yesterday.toDateString()) return `昨天 ${formatShortTime(value)}`;
  return `${date.getMonth() + 1}月${date.getDate()}日 ${formatShortTime(value)}`;
}

export function knowledgeTitle(path: string): string {
  const name = path.split("/").pop()?.replace(/\.md$/i, "") || path;
  return name.replace(/[-_]+/g, " ");
}

export function formatShortDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  return `${date.getMonth() + 1}月${date.getDate()}日`;
}

export function formatShortTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
}
