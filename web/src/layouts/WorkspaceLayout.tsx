import {
  ApiOutlined,
  BellOutlined,
  BookOutlined,
  CheckCircleOutlined,
  ClusterOutlined,
  CodeOutlined,
  DatabaseOutlined,
  DownOutlined,
  FileMarkdownOutlined,
  FileSearchOutlined,
  HomeOutlined,
  InboxOutlined,
  NodeIndexOutlined,
  PlayCircleOutlined,
  ReloadOutlined,
  SearchOutlined,
  SettingOutlined,
  SyncOutlined,
  ToolOutlined,
  WarningOutlined,
} from "@ant-design/icons";
import { PageContainer, ProCard, ProLayout } from "@ant-design/pro-components";
import { Alert, Avatar, Badge, Button, Divider, Dropdown, Flex, Progress, Segmented, Skeleton, Tooltip, Typography } from "antd";
import { createContext, Suspense, useContext } from "react";
import type { ReactNode } from "react";
import { Link, Outlet, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { surfaceFromPath } from "../app/navigation";
import { useWorkspace } from "../app/WorkspaceContext";
import { FilePreviewDrawer } from "../shared/FilePreviewDrawer";
import type { BootstrapStatus } from "../types/api";
import { formatDateTime } from "../shared/utils";
import styles from "./WorkspaceLayout.module.css";

const { Text, Title } = Typography;

type WorkspaceOutletValue = {
  openFile: (path: string) => void;
};

const WorkspaceOutletContext = createContext<WorkspaceOutletValue | null>(null);

const browseMenu = [
  { path: "/", name: "首页", icon: <HomeOutlined /> },
  { path: "/query", name: "搜索与提问", icon: <SearchOutlined /> },
  { path: "/topics", name: "主题与实体", icon: <NodeIndexOutlined /> },
  { path: "/code", name: "系统与代码", icon: <CodeOutlined /> },
  { path: "/collections", name: "收藏与集合", icon: <BookOutlined /> },
  { path: "/wiki", name: "Wiki 知识库", icon: <FileMarkdownOutlined /> },
  { path: "/sources", name: "来源与文档", icon: <FileSearchOutlined /> },
  { path: "/graph", name: "知识图谱", icon: <ClusterOutlined /> },
];

const adminMenu = [
  { path: "/admin", name: "维护总览", icon: <HomeOutlined /> },
  { path: "/admin/sources", name: "统一来源库", icon: <InboxOutlined /> },
  { path: "/admin/wiki", name: "Wiki 治理", icon: <FileMarkdownOutlined /> },
  { path: "/admin/repositories", name: "代码仓库", icon: <CodeOutlined /> },
  { path: "/admin/reviews", name: "审阅中心", icon: <WarningOutlined /> },
  { path: "/admin/jobs", name: "任务中心", icon: <PlayCircleOutlined /> },
  { path: "/admin/quality", name: "质量治理", icon: <CheckCircleOutlined /> },
  { path: "/admin/settings", name: "系统诊断", icon: <ToolOutlined /> },
];

export function WorkspaceLayout() {
  const { api, settings, startup, reloadWorkspace } = useWorkspace();
  const location = useLocation();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const surface = surfaceFromPath(location.pathname);
  const previewPath = searchParams.get("preview");
  const workspaceName = settings.projectID || "本地知识库";
  const updatedAt = formatKnowledgeDate(startup.kind === "loading" || startup.kind === "error" ? "" : startup.status.time);

  if (startup.kind === "loading" || startup.kind === "error") {
    return (
      <main className={styles.startup}>
        <ProCard className={styles.startupCard}>
          <Flex vertical align="center" gap={20}>
            <DatabaseOutlined className={styles.startupIcon} />
            <Title level={2}>Knowledge Core</Title>
            {startup.kind === "loading" ? (
              <Alert type="info" showIcon message="正在读取后端配置并加载活动项目…" />
            ) : (
              <>
                <Alert type="error" showIcon message="无法加载活动项目" description={startup.message} />
                <Text type="secondary">后端初始化期间会自动重试；持续失败时请检查 config.yaml。</Text>
                <Button type="primary" icon={<ReloadOutlined />} onClick={() => void reloadWorkspace()}>
                  重试
                </Button>
              </>
            )}
          </Flex>
        </ProCard>
      </main>
    );
  }

  const openFile = (path: string) => {
    const next = new URLSearchParams(searchParams);
    next.set("preview", path);
    setSearchParams(next);
  };
  const closePreview = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("preview");
    setSearchParams(next);
  };

  return (
    <WorkspaceOutletContext.Provider value={{ openFile }}>
      <ProLayout
        className={styles.shell}
        title="Knowledge Core"
        logo={<DatabaseOutlined />}
        route={{ routes: surface === "browse" ? browseMenu : adminMenu }}
        location={{ pathname: location.pathname }}
        fixSiderbar
        fixedHeader
        breakpoint="lg"
        siderWidth={232}
        menuItemRender={(item, dom) => item.path ? <Link to={item.path}>{dom}</Link> : dom}
        actionsRender={false}
        menuFooterRender={(menuProps) => {
          const label = startup.kind === "ready" ? "知识库可用" : "正在构建";
          const collapsed = Boolean(menuProps?.collapsed);
          return (
            <Tooltip title={collapsed ? label : undefined} placement="right">
              <Flex
                className={`${styles.menuStatus} ${collapsed ? styles.menuStatusCollapsed : ""}`}
                align="center"
                justify={collapsed ? "center" : "flex-start"}
                gap={8}
                aria-label={`知识库状态：${label}`}
              >
                <Badge status={startup.kind === "ready" ? "success" : "processing"} />
                {!collapsed && <Text className={styles.menuStatusText}>{label}</Text>}
              </Flex>
            </Tooltip>
          );
        }}
      >
        <header className={styles.workspaceHeader} aria-label="工作区导航">
          <Flex className={styles.workspaceHeaderInner} align="center" justify="flex-end">
            <Flex className={styles.headerActions} align="center" gap={10}>
              <Segmented
                className={styles.surfaceSwitch}
                value={surface}
                options={[
                  {
                    label: <span className={styles.surfaceOption}><BookOutlined /><span className={styles.surfaceText}>知识浏览</span></span>,
                    value: "browse",
                  },
                  {
                    label: <span className={styles.surfaceOption}><SettingOutlined /><span className={styles.surfaceText}>管理后台</span></span>,
                    value: "admin",
                  },
                ]}
                onChange={(value) => navigate(value === "admin" ? "/admin" : "/")}
              />
              <Divider type="vertical" className={styles.headerDivider} />
              <Tooltip title={surface === "browse" ? `知识证据更新至 ${updatedAt}` : "服务与管理能力运行正常"}>
                <Flex className={styles.freshness} align="center" gap={9}>
                  <Badge status="success" />
                  <Flex vertical className={styles.freshnessCopy}>
                    <Text strong>{surface === "browse" ? "知识已更新" : "服务运行正常"}</Text>
                    <Text type="secondary">{surface === "browse" ? updatedAt : "管理能力可用"}</Text>
                  </Flex>
                  {surface === "browse" ? <SyncOutlined className={styles.freshnessIcon} /> : <ApiOutlined className={styles.freshnessIcon} />}
                </Flex>
              </Tooltip>
              <Tooltip title="通知">
                <Badge dot offset={[-3, 5]}>
                  <Button className={styles.noticeButton} type="text" shape="circle" aria-label="通知" icon={<BellOutlined />} />
                </Badge>
              </Tooltip>
              <Dropdown
                trigger={["click"]}
                menu={{
                  items: [
                    {
                      key: "workspace",
                      disabled: true,
                      label: (
                        <Flex vertical>
                          <Text strong>{workspaceName}</Text>
                          <Text type="secondary">当前知识空间</Text>
                        </Flex>
                      ),
                    },
                    { type: "divider" },
                    { key: "settings", icon: <ToolOutlined />, label: "系统诊断" },
                  ],
                  onClick: ({ key }) => {
                    if (key === "settings") navigate("/admin/settings");
                  },
                }}
              >
                <Button type="text" className={styles.workspaceButton} aria-label={`当前知识空间：${workspaceName}`}>
                  <Avatar shape="square" className={styles.workspaceAvatar} icon={<DatabaseOutlined />} />
                  <Flex vertical className={styles.workspaceCopy}>
                    <Text strong ellipsis={{ tooltip: workspaceName }}>{workspaceName}</Text>
                    <Text type="secondary">当前知识空间</Text>
                  </Flex>
                  <DownOutlined className={styles.workspaceChevron} />
                </Button>
              </Dropdown>
            </Flex>
          </Flex>
        </header>
        <PageContainer title={false} className={styles.content}>
          {startup.kind === "bootstrapping" && (
            <ProCard className={styles.bootstrap}>
              <BootstrapProgressPanel bootstrap={startup.status.bootstrap!} />
            </ProCard>
          )}
          <Suspense fallback={<ProCard><Skeleton active /></ProCard>}>
            <Outlet />
          </Suspense>
        </PageContainer>
        <FilePreviewDrawer
          api={api}
          settings={settings}
          path={previewPath}
          onClose={closePreview}
          onOpenFile={openFile}
        />
      </ProLayout>
    </WorkspaceOutletContext.Provider>
  );
}

export function useWorkspaceOutlet(): WorkspaceOutletValue {
  const value = useContext(WorkspaceOutletContext);
  if (!value) throw new Error("useWorkspaceOutlet must be used inside WorkspaceLayout");
  return value;
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
    conflict_requeued: "页面变化，重新处理来源",
    impact_review: "判断共享页面影响",
    impact_requeued: "受影响来源重新入队",
    pg_pending: "等待 PostgreSQL 同步",
    completed: "章节完成",
    skipped: "跳过已完成章节",
    synthesizing_overview: "生成全局 Overview",
    syncing_pg: "同步 PostgreSQL / Embedding",
    retry_wait: "等待自动重试",
  };

  return (
    <Flex vertical gap={12}>
      <Title level={4}>正在构建知识库</Title>
      <Progress percent={percent} status={bootstrap.status === "retrying" ? "exception" : "active"} />
      <Text strong>{bootstrap.completed_sources} / {bootstrap.total_sources} 个来源已完成</Text>
      {(bootstrap.restored_sources ?? 0) > 0 && (
        <Text type="secondary">其中断点恢复 {bootstrap.restored_sources} 个，本次完成 {bootstrap.completed_this_run ?? 0} 个</Text>
      )}
      <Text type="secondary">阶段：{stageLabels[bootstrap.stage] ?? bootstrap.stage}</Text>
      {bootstrap.active_sources && bootstrap.active_sources.length > 0 && (
        <Flex vertical gap={4}>
          <Text type="secondary">并发任务：{bootstrap.active_sources.length}</Text>
          {bootstrap.active_sources.map((source) => (
            <Text key={source.path} type="secondary" ellipsis={{ tooltip: source.path }}>
              {source.number}/{bootstrap.total_sources} · 第 {source.attempt || 1} 次 · {stageLabels[source.phase] ?? source.phase} · {source.path}
            </Text>
          ))}
        </Flex>
      )}
      {(bootstrap.requeued_sources ?? 0) > 0 && (
        <Text type="secondary">
          已重新入队 {bootstrap.requeued_sources} 次（冲突 {bootstrap.conflict_requeues ?? 0}、影响 {bootstrap.impact_requeues ?? 0}、失败 {bootstrap.failure_requeues ?? 0}）；
          等待冲突收敛 {bootstrap.queued_conflict_sources ?? 0} 个；
          影响检查 {bootstrap.impact_checks ?? 0} 次
        </Text>
      )}
      {(bootstrap.llm_calls ?? 0) > 0 && (
        <Text type="secondary">
          LLM 请求 {bootstrap.llm_calls} 次，失败 {bootstrap.llm_failures ?? 0} 次；来源平均耗时 {Math.round((bootstrap.average_source_duration_ms ?? 0) / 1000)} 秒，最慢 {Math.round((bootstrap.max_source_duration_ms ?? 0) / 1000)} 秒
        </Text>
      )}
      {(bootstrap.llm_in_flight ?? 0) > 0 && (
        <Text type="secondary">LLM 正在处理 {bootstrap.llm_in_flight} 个请求，最久已运行 {Math.round((bootstrap.oldest_llm_call_ms ?? 0) / 1000)} 秒</Text>
      )}
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
    </Flex>
  );
}

function formatKnowledgeDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "时间未知";
  const options: Intl.DateTimeFormatOptions = { month: "long", day: "numeric" };
  if (date.getFullYear() !== new Date().getFullYear()) options.year = "numeric";
  return new Intl.DateTimeFormat("zh-CN", options).format(date);
}
