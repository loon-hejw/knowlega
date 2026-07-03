export type AgentName = "auto" | "mock" | "llm" | "fallback";

export interface AppSettings {
  apiBaseUrl: string;
  projectPath: string;
  projectID: string;
  agent: AgentName;
}

export interface HealthResponse {
  ok: boolean;
  service: string;
  time: string;
}

export interface IngestTask {
  id: string;
  source_path: string;
  title?: string;
  status: "pending" | "processing" | "done" | "failed";
  sha256?: string;
  files?: string[];
  raw_path?: string;
  error?: string;
  retry_count: number;
  added_at: string;
  updated_at: string;
}

export interface IngestQueue {
  version: number;
  tasks: IngestTask[];
}

export interface RunQueueResult {
  processed: number;
  done: number;
  failed: number;
  skipped: number;
  files: number;
  tasks: IngestTask[];
}

export interface ScanSourcesResult {
  queued: number;
  skipped: number;
  tasks: IngestTask[];
}

export interface QueueSourceResponse {
  task: IngestTask;
}

export interface QuerySearch {
  text: string;
  weight: number;
  rationale: string;
}

export interface QueryPlan {
  question: string;
  intent: string;
  read_first: string[] | null;
  searches: QuerySearch[] | null;
  candidate_limit: number;
  answer_mode: string;
  can_write_back: boolean;
}

export interface QueryAction {
  action: string;
  path?: string;
  query?: string;
  limit?: number;
  title?: string;
  answer?: string;
  rationale?: string;
}

export interface QueryTraceStep {
  step: number;
  action: QueryAction;
  observation: string;
}

export interface QueryCitation {
  path: string;
  title: string;
  kind: string;
}

export interface QueryResult {
  path: string;
  title: string;
  snippet: string;
  score: number;
  kind: string;
}

export interface QueryAnswer {
  question: string;
  plan: QueryPlan;
  results: QueryResult[] | null;
  answer: string;
  suggested_writeback_title?: string;
  citations: QueryCitation[] | null;
  trace?: QueryTraceStep[];
  notes: string[] | null;
}

export interface QueryWriteback {
  path: string;
  title: string;
}

export interface QueryResponseWithWriteback {
  answer: QueryAnswer;
  writeback: QueryWriteback;
}

export interface ReviewItem {
  ID: string;
  ProjectID: string;
  Type: string;
  Title: string;
  Description: string;
  Severity: string;
  Status: string;
  AffectedPages: string[];
  CreatedAt: string;
  ResolvedAt: string | null;
}

export interface ReviewsResponse {
  reviews: ReviewItem[];
  count: number;
}

export interface LintIssue {
  Type: string;
  Path: string;
  Detail: string;
}

export interface IssuesResponse {
  issues: LintIssue[];
  count: number;
}

export interface ValidateWikiResult {
  SourceCount: number;
  FileCount: number;
  ReviewCount: number;
  SkippedCount: number;
  Results: Array<{
    RawPath: string;
    Analysis: string;
    Files: string[];
    ReviewCount: number;
    Skipped: boolean;
    SHA256: string;
  }>;
}

export interface SyncWikiResult {
  Pages: number;
  Versions: number;
  Reviews: number;
  Sources: number;
  SourceManifestEntries: number;
  Embeddings: number;
}
