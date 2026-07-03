import type {
  AgentName,
  HealthResponse,
  IngestQueue,
  IssuesResponse,
  QueryAnswer,
  QueryResponseWithWriteback,
  QueueSourceResponse,
  ReviewsResponse,
  RunQueueResult,
  ScanSourcesResult,
  SyncWikiResult,
  ValidateWikiResult,
} from "../types/api";

type RequestOptions = {
  method?: "GET" | "POST";
  body?: unknown;
  query?: Record<string, string | number | boolean | undefined>;
};

export class ApiClient {
  constructor(private readonly baseUrl: string) {}

  health(): Promise<HealthResponse> {
    return this.request<HealthResponse>("/health");
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
  }): Promise<{ review: unknown }> {
    return this.request("/reviews/resolve", { method: "POST", body: input });
  }

  syncWikiPG(input: {
    project_path?: string;
    project_id?: string;
  }): Promise<SyncWikiResult> {
    return this.request("/wiki/sync-pg", { method: "POST", body: input });
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
    const text = await response.text();
    const data = text ? JSON.parse(text) : null;
    if (!response.ok) {
      const message =
        data && typeof data.error === "string" ? data.error : `HTTP ${response.status}`;
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
