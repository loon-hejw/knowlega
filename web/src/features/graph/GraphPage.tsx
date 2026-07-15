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

export function GraphPage({
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

export function GraphNodeTable({
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

export function GraphRepositoriesPanel({
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

export function GraphInsightsPanel({
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

export function ResearchJobsPanel({
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

export function UnifiedGraphCanvas({
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

export function mergeGraphResponses(current: WikiGraphResponse | null, incoming: WikiGraphResponse): WikiGraphResponse {
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

export function normalizeGraphResponse(graph: WikiGraphResponse): WikiGraphResponse {
  return { ...graph, nodes: graph.nodes ?? [], edges: graph.edges ?? [] };
}

export function domainFill(domain: string): string {
  return { wiki: "#dbeafe", source: "#fef3c7", code: "#d1fae5" }[domain] ?? "#e5e7eb";
}

export function domainStroke(domain: string): string {
  return { wiki: "#2563eb", source: "#d97706", code: "#059669" }[domain] ?? "#64748b";
}

export function domainColor(domain: string): string {
  return { wiki: "blue", source: "gold", code: "green" }[domain] ?? "default";
}

export function confidenceStroke(confidence: string): string {
  return { EXTRACTED: "#64748b", INFERRED: "#7c3aed", AMBIGUOUS: "#d97706" }[confidence] ?? "#94a3b8";
}

export function confidenceColor(confidence: string): string {
  return { EXTRACTED: "green", INFERRED: "purple", AMBIGUOUS: "orange" }[confidence] ?? "default";
}
