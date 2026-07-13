import type {
  AgentName,
  ChatAppendResponse,
  ChatResponse,
  ChatRun,
  ChatRunEvent,
  ChatRunResponse,
  ChatsResponse,
  HealthResponse,
  IngestQueue,
  IssuesResponse,
  DeleteSourceResult,
  ProjectFileContent,
  ProjectFilesResponse,
  ProjectGraphQuery,
  GraphNodeDetailResponse,
  GraphRepositoriesResponse,
  GraphJobsResponse,
  GraphIndexJob,
  QueryAnswer,
  QueryResponseWithWriteback,
  QueueSourceResponse,
  ReviewActionResponse,
  ResearchJobResponse,
  ResearchJobsResponse,
  ReviewSweepResponse,
  ReviewsResponse,
  RunQueueResult,
  ScanSourcesResult,
  SourcesManifestResponse,
  SourceLayoutMigrationResult,
  SyncWikiResult,
  UploadSourcesResponse,
  ValidateWikiResult,
  WorkspaceJobResponse,
  WorkspaceJobsResponse,
  WorkspaceStatus,
  WikiGraphResponse,
  WikiGraphInsightsResponse,
  WriteProjectFileContentResult,
} from "../types/api";

type RequestOptions = {
  method?: "GET" | "POST" | "PUT" | "DELETE";
  body?: unknown;
  query?: Record<string, string | number | boolean | undefined>;
};

export class ApiClient {
  constructor(private readonly baseUrl: string) {}

  health(): Promise<HealthResponse> {
    return this.request<HealthResponse>("/health");
  }

  workspaceStatus(input: {
    project_path?: string;
    project_id?: string;
    agent?: AgentName;
  }): Promise<WorkspaceStatus> {
    return this.request<WorkspaceStatus>("/workspace/status", {
      query: {
        project: input.project_path,
        project_id: input.project_id,
        agent: input.agent,
      },
    });
  }

  maintainWorkspace(input: {
    project_path?: string;
    project_id?: string;
    agent?: AgentName;
    skip_unchanged?: boolean;
    retry_failed?: boolean;
    keep_done?: boolean;
    run_llm_review?: boolean;
    run_pg_sync?: boolean;
  }): Promise<WorkspaceJobResponse> {
    return this.request<WorkspaceJobResponse>("/workspace/maintain", {
      method: "POST",
      body: input,
    });
  }

  workspaceJobs(projectPath?: string): Promise<WorkspaceJobsResponse> {
    return this.request<WorkspaceJobsResponse>("/workspace/jobs", {
      query: { project: projectPath },
    });
  }

  workspaceJob(projectPath: string | undefined, id: string): Promise<WorkspaceJobResponse> {
    return this.request<WorkspaceJobResponse>(`/workspace/jobs/${encodeURIComponent(id)}`, {
      query: { project: projectPath },
    });
  }

  initProject(path: string, name: string): Promise<{ path: string }> {
    return this.request("/projects/init", {
      method: "POST",
      body: { path, name },
    });
  }

  queueSource(input: {
    project_path?: string;
    source_path: string;
    title?: string;
  }): Promise<QueueSourceResponse> {
    return this.request("/sources/queue", { method: "POST", body: input });
  }

  scanSources(projectPath?: string): Promise<ScanSourcesResult> {
    return this.request("/sources/scan", {
      method: "POST",
      body: { project_path: projectPath },
    });
  }

  queueTasks(projectPath?: string): Promise<IngestQueue> {
    return this.request("/queue/tasks", {
      query: { project: projectPath },
    });
  }

  sources(projectPath?: string): Promise<SourcesManifestResponse> {
    return this.request("/projects/sources", {
      query: { project: projectPath },
    });
  }

  uploadSources(formData: FormData): Promise<UploadSourcesResponse> {
    return this.requestForm("/projects/sources/upload", formData);
  }

  sourceLayoutPlan(projectPath?: string): Promise<SourceLayoutMigrationResult> {
    return this.request("/projects/sources/layout-migration", { query: { project: projectPath, plan: true } });
  }

  migrateSourceLayout(projectPath?: string): Promise<SourceLayoutMigrationResult> {
    return this.request("/projects/sources/layout-migration", { method: "POST", body: { project_path: projectPath, apply: true } });
  }

  projectFiles(projectPath?: string): Promise<ProjectFilesResponse> {
    return this.request("/projects/files", {
      query: { project: projectPath },
    });
  }

  projectFileContent(projectPath: string | undefined, path: string): Promise<ProjectFileContent> {
    return this.request("/projects/files/content", {
      query: { project: projectPath, path },
    });
  }

  writeProjectFileContent(input: {
    project_path?: string;
    path: string;
    content: string;
    reason?: string;
  }): Promise<WriteProjectFileContentResult> {
    return this.request("/projects/files/content", { method: "PUT", body: input });
  }

  wikiGraph(projectPath?: string): Promise<WikiGraphResponse> {
    return this.request("/projects/graph", {
      query: { project: projectPath },
    });
  }

  projectGraph(input: ProjectGraphQuery): Promise<WikiGraphResponse> {
    return this.request("/projects/graph/query", { method: "POST", body: input });
  }

  graphNode(projectPath: string | undefined, id: string): Promise<GraphNodeDetailResponse> {
    return this.request("/projects/graph/node", { query: { project: projectPath, id } });
  }

  graphRepositories(): Promise<GraphRepositoriesResponse> {
    return this.request("/projects/graph/repos");
  }

  graphJobs(): Promise<GraphJobsResponse> {
    return this.request("/projects/graph/jobs");
  }

  queueGraphJob(input: { registry_id: string; repository_id: string; branch?: string }): Promise<{ job: GraphIndexJob }> {
    return this.request("/projects/graph/jobs", { method: "POST", body: input });
  }

  wikiGraphInsights(projectPath?: string): Promise<WikiGraphInsightsResponse> {
    return this.request("/projects/graph/insights", {
      query: { project: projectPath },
    });
  }

  deleteSource(input: {
    project_path?: string;
    project_id?: string;
    source_path: string;
    delete_raw?: boolean;
    dry_run?: boolean;
  }): Promise<DeleteSourceResult> {
    return this.request("/projects/sources/delete", { method: "POST", body: input });
  }

  runQueue(input: {
    project_path?: string;
    project_id?: string;
    agent?: AgentName;
    skip_unchanged?: boolean;
    max?: number;
    retry_failed?: boolean;
    keep_done?: boolean;
  }): Promise<RunQueueResult> {
    return this.request("/queue/run", { method: "POST", body: input });
  }

  query(input: {
    project_path?: string;
    project_id?: string;
    q: string;
    limit?: number;
    agent?: AgentName;
    save_title?: string;
  }): Promise<QueryAnswer | QueryResponseWithWriteback> {
    return this.request("/query", { method: "POST", body: input });
  }

  chats(projectPath?: string): Promise<ChatsResponse> {
    return this.request("/chats", { query: { project: projectPath } });
  }

  createChat(input: { project_path?: string; title?: string }): Promise<ChatResponse> {
    return this.request("/chats", { method: "POST", body: input });
  }

  chat(projectPath: string | undefined, id: string): Promise<ChatResponse> {
    return this.request(`/chats/${encodeURIComponent(id)}`, { query: { project: projectPath } });
  }

  deleteChat(projectPath: string | undefined, id: string): Promise<{ deleted: boolean }> {
    return this.request(`/chats/${encodeURIComponent(id)}`, {
      method: "DELETE",
      query: { project: projectPath },
    });
  }

  appendChatMessage(input: {
    project_path?: string;
    project_id?: string;
    chat_id: string;
    q: string;
    limit?: number;
    agent?: AgentName;
    save_title?: string;
  }): Promise<ChatAppendResponse> {
    return this.request(`/chats/${encodeURIComponent(input.chat_id)}/messages`, {
      method: "POST",
      body: input,
    });
  }

  startChatRun(input: {
    project_path?: string;
    project_id?: string;
    chat_id: string;
    q: string;
    limit?: number;
    agent?: AgentName;
    save_title?: string;
  }): Promise<ChatRunResponse> {
    return this.request(`/chats/${encodeURIComponent(input.chat_id)}/runs`, {
      method: "POST",
      body: input,
    });
  }

  cancelChatRun(projectPath: string | undefined, chatID: string, runID: string): Promise<{ run: ChatRun }> {
    return this.request(`/chats/${encodeURIComponent(chatID)}/runs/${encodeURIComponent(runID)}/cancel`, {
      method: "POST",
      query: { project: projectPath },
    });
  }

  async streamChatRunEvents(input: {
    project_path?: string;
    chat_id: string;
    run_id: string;
    signal?: AbortSignal;
    onEvent: (event: ChatRunEvent) => void;
  }): Promise<void> {
    const response = await fetch(
      this.buildUrl(`/chats/${encodeURIComponent(input.chat_id)}/runs/${encodeURIComponent(input.run_id)}/events`, {
        project: input.project_path,
      }),
      { signal: input.signal },
    );
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    if (!response.body) {
      throw new Error("浏览器不支持流式响应");
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const chunks = buffer.split("\n\n");
      buffer = chunks.pop() ?? "";
      for (const chunk of chunks) {
        const event = parseSSEChunk(chunk);
        if (event) input.onEvent(event);
      }
    }
    if (buffer.trim()) {
      const event = parseSSEChunk(buffer);
      if (event) input.onEvent(event);
    }
  }

  lint(projectPath?: string): Promise<IssuesResponse> {
    return this.request("/projects/lint", {
      query: { project: projectPath },
    });
  }

  validateWiki(input: {
    project_path?: string;
    project_id?: string;
    source_path: string;
    title?: string;
    agent?: AgentName;
    skip_unchanged?: boolean;
  }): Promise<ValidateWikiResult> {
    return this.request("/wiki/validate", { method: "POST", body: input });
  }

  wikiReview(input: {
    project_path?: string;
    agent?: AgentName;
  }): Promise<IssuesResponse> {
    return this.request("/wiki/review", { method: "POST", body: input });
  }

  reviews(projectPath?: string, projectID?: string, status?: string): Promise<ReviewsResponse> {
    return this.request("/reviews", {
      query: { project: projectPath, project_id: projectID, status },
    });
  }

  resolveReview(input: {
    project_path?: string;
    project_id?: string;
    id: string;
    status: "resolved" | "dismissed" | "open";
    action?: string;
  }): Promise<{ review: unknown }> {
    return this.request("/reviews/resolve", { method: "POST", body: input });
  }

  reviewAction(input: {
    project_path?: string;
    project_id?: string;
    id: string;
    action: string;
    agent?: AgentName;
  }): Promise<ReviewActionResponse> {
    return this.request("/reviews/action", { method: "POST", body: input });
  }

  sweepReviews(input: {
    project_path?: string;
    project_id?: string;
    agent?: AgentName;
  }): Promise<ReviewSweepResponse> {
    return this.request("/reviews/sweep", { method: "POST", body: input });
  }

  createResearchJob(input: {
    project_path?: string;
    project_id?: string;
    review_id?: string;
    topic?: string;
    query?: string;
    agent?: AgentName;
  }): Promise<ResearchJobResponse> {
    return this.request("/research/jobs", { method: "POST", body: input });
  }

  researchJobs(projectPath?: string): Promise<ResearchJobsResponse> {
    return this.request("/research/jobs", { query: { project: projectPath } });
  }

  researchJob(projectPath: string | undefined, id: string): Promise<ResearchJobResponse> {
    return this.request(`/research/jobs/${encodeURIComponent(id)}`, {
      query: { project: projectPath },
    });
  }

  syncWikiPG(input: {
    project_path?: string;
    project_id?: string;
  }): Promise<SyncWikiResult> {
    return this.request("/wiki/sync-pg", { method: "POST", body: input });
  }

  private async requestForm<T>(path: string, body: FormData): Promise<T> {
    const response = await fetch(this.buildUrl(path), {
      method: "POST",
      body,
    });
    return this.parseResponse<T>(response);
  }

  private async request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const url = this.buildUrl(path, options.query);
    const response = await fetch(url, {
      method: options.method ?? "GET",
      headers:
        options.body === undefined
          ? undefined
          : {
              "Content-Type": "application/json",
            },
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
    });
    return this.parseResponse<T>(response);
  }

  private async parseResponse<T>(response: Response): Promise<T> {
    const text = await response.text();
    let data: unknown = null;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch {
        throw new Error(`API 返回了非 JSON 内容：${text.slice(0, 160)}`);
      }
    }
    if (!response.ok) {
      const message =
        data && typeof data === "object" && "error" in data && typeof data.error === "string"
          ? data.error
          : `HTTP ${response.status}`;
      throw new Error(message);
    }
    return data as T;
  }

  private buildUrl(path: string, query?: RequestOptions["query"]): string {
    const base = this.baseUrl.replace(/\/+$/, "");
    const normalizedPath = path.startsWith("/") ? path : `/${path}`;
    const url = new URL(`${base}${normalizedPath}`, window.location.origin);
    for (const [key, value] of Object.entries(query ?? {})) {
      if (value !== undefined && value !== "") {
        url.searchParams.set(key, String(value));
      }
    }
    return url.toString();
  }
}

export function isQueryWritebackResponse(
  value: QueryAnswer | QueryResponseWithWriteback,
): value is QueryResponseWithWriteback {
  return "answer" in value && "writeback" in value;
}

function parseSSEChunk(chunk: string): ChatRunEvent | null {
  const dataLines = chunk
    .split(/\r?\n/)
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice("data:".length).trimStart());
  if (dataLines.length === 0) {
    return null;
  }
  return JSON.parse(dataLines.join("\n")) as ChatRunEvent;
}
