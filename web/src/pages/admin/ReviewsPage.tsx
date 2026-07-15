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


import { ResearchJobsPanel } from "../../features/graph/GraphPage";
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


export function ReviewsPage({
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
export function ReviewPagePaths({ pages }: { pages: string[] }) {
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

export function ReviewDetailDrawer({
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

export function ReviewQueries({ queries }: { queries: string[] }) {
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
