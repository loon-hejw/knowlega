export type AgentName = "llm";

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
  ready?: boolean;
  bootstrap?: BootstrapStatus;
}

export interface BootstrapStatus {
  status: "pending" | "running" | "retrying" | "succeeded" | "failed";
  stage: string;
  project_path: string;
  current_source?: string;
  current_source_num: number;
  completed_sources: number;
  total_sources: number;
  files: number;
  reviews: number;
  attempt: number;
  error?: string;
  next_retry_at?: string;
  started_at: string;
  updated_at: string;
  finished_at?: string;
}

export interface WorkspaceFileStatus {
  path: string;
  exists: boolean;
}

export interface WorkspaceQueueStatus {
  pending: number;
  processing: number;
  done: number;
  failed: number;
  total: number;
}

export interface WorkspaceReviewStatus {
  open: number;
  resolved: number;
  dismissed: number;
  total: number;
}

export interface WorkspaceLintStatus {
  count: number;
  error?: string;
}

export interface WorkspaceStatus {
  ok: boolean;
  service: string;
  time: string;
  project_path: string;
  project_id: string;
  agent: string;
  files: WorkspaceFileStatus[];
  queue: WorkspaceQueueStatus;
  reviews: WorkspaceReviewStatus;
  lint: WorkspaceLintStatus;
  pg_configured: boolean;
  embedding_configured: boolean;
  bootstrap?: BootstrapStatus;
}

export interface MaintainWikiStep {
  name: string;
  status: "ok" | "failed" | "skipped";
  started_at: string;
  finished_at: string;
  summary?: Record<string, unknown>;
  detail?: unknown;
  error?: string;
}

export interface MaintainWikiResult {
  status: "ok" | "failed";
  started_at: string;
  finished_at: string;
  steps: MaintainWikiStep[];
}

export interface WorkspaceJob {
  id: string;
  project_path: string;
  project_id: string;
  kind: string;
  status: "queued" | "running" | "succeeded" | "failed";
  request: {
    project_path?: string;
    project_id?: string;
    agent?: AgentName;
    skip_unchanged?: boolean;
    retry_failed?: boolean;
    keep_done?: boolean;
    run_llm_review?: boolean;
    run_pg_sync?: boolean;
  };
  result?: MaintainWikiResult;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface WorkspaceJobResponse {
  job: WorkspaceJob;
  existing?: boolean;
}

export interface WorkspaceJobsResponse {
  jobs: WorkspaceJob[];
  count: number;
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

export interface SourceManifestEntry {
  key: string;
  original_path: string;
  raw_path: string;
  archive_path?: string;
  original_raw_path?: string;
  content_path?: string;
  title: string;
  sha256: string;
  files: string[];
  review_count: number;
  updated_at: string;
}

export interface SourcesManifestResponse {
  sources: SourceManifestEntry[];
  count: number;
}

export interface UploadedSourceFile {
  path: string;
  archive_path: string;
  original_path: string;
  content_path: string;
  size: number;
  sha256: string;
  queued_task_id?: string;
}

export interface UnsupportedUploadedSource {
  path: string;
  reason: string;
}

export interface UploadSourcesResponse {
  uploaded: UploadedSourceFile[];
  queued: number;
  unsupported?: UnsupportedUploadedSource[];
}

export interface SourceLayoutMigrationItem {
  manifest_key?: string;
  original_source: string;
  original_name: string;
  old_paths: string[];
  target_archive_path: string;
  target_original_path: string;
  target_content_path: string;
  sha256: string;
  stage: string;
  error?: string;
}

export interface SourceLayoutMigrationResult {
  status: string;
  dry_run: boolean;
  total: number;
  completed: number;
  blocked: number;
  journal_path: string;
  items: SourceLayoutMigrationItem[];
}

export interface ProjectFile {
  path: string;
  kind: string;
  size: number;
  mod_time: string;
}

export interface ProjectFilesResponse {
  files: ProjectFile[];
  count: number;
}

export interface ProjectFileContent {
  path: string;
  kind: string;
  size: number;
  mod_time: string;
  title?: string;
  type?: string;
  sources?: string[];
  frontmatter?: Record<string, unknown>;
  content: string;
}

export interface WriteProjectFileContentResult {
  path: string;
  versioned: boolean;
  updated_at: string;
}

export interface WikiGraphNode {
  id: string;
  path: string;
  title: string;
  type: string;
  domain: "wiki" | "source" | "code" | string;
  kind: string;
  label: string;
  scope_id?: string;
  community?: string;
  source_ref?: string;
  sources: string[];
  in_degree: number;
  out_degree: number;
  props?: Record<string, unknown>;
}

export interface WikiGraphEdge {
  id: string;
  source: string;
  target: string;
  kind: string;
  relation: string;
  confidence: "EXTRACTED" | "INFERRED" | "AMBIGUOUS" | string;
  confidence_score: number;
  weight: number;
  evidence?: string[];
  props?: Record<string, unknown>;
}

export interface WikiGraphResponse {
  nodes: WikiGraphNode[];
  edges: WikiGraphEdge[];
  truncated?: boolean;
  next_cursor?: string;
  stats?: {
    total_nodes: number;
    total_edges: number;
    by_domain: Record<string, number>;
    by_kind: Record<string, number>;
  };
}

export interface ProjectGraphQuery {
  project_path?: string;
  seed_ids?: string[];
  query?: string;
  domains?: string[];
  kinds?: string[];
  relations?: string[];
  confidence?: string[];
  min_confidence_score?: number;
  depth?: number;
  limit?: number;
  cursor?: string;
}

export interface GraphNodeDetailResponse {
  node: WikiGraphNode;
  edges: WikiGraphEdge[];
}

export interface GraphRepositoryStatus {
  registry_id: string;
  provider: string;
  repository_id: string;
  full_name: string;
  branch: string;
  disabled: boolean;
  indexed: boolean;
  commit?: string;
  dirty?: boolean;
  source_sha256?: string;
  snapshot_path?: string;
}

export interface GraphRepositoriesResponse {
  repositories: GraphRepositoryStatus[];
  enabled: boolean;
  count?: number;
}

export interface GraphIndexJob {
  id: string;
  registry_id: string;
  repository_id: string;
  full_name: string;
  branch: string;
  commit?: string;
  delivery_id?: string;
  trigger: string;
  status: "pending" | "running" | "done" | "failed" | "superseded";
  attempts: number;
  error?: string;
  snapshot_path?: string;
  node_count?: number;
  edge_count?: number;
  created_at: string;
  updated_at: string;
}

export interface GraphJobsResponse {
  jobs: GraphIndexJob[];
  enabled: boolean;
  count?: number;
}

export interface WikiGraphInsight {
  id?: string;
  domain?: string;
  path: string;
  title: string;
  type: string;
  in_degree: number;
  out_degree: number;
  reason: string;
  query?: string;
  score?: number;
}

export interface WikiGraphInsightsResponse {
  isolated_pages: WikiGraphInsight[];
  hub_pages: WikiGraphInsight[];
  missing_sources: WikiGraphInsight[];
  bridge_candidates: WikiGraphInsight[];
  god_nodes?: WikiGraphInsight[];
  surprising_connections?: WikiGraphInsight[];
  suggested_questions?: string[];
}

export interface DeleteSourceResult {
  matched_key: string;
  original_path: string;
  raw_path: string;
  title: string;
  deleted_pages: string[];
  updated_pages: string[];
  cleaned_pages: string[];
  resolved_reviews: string[];
  deleted_raw: boolean;
  manifest_updated: boolean;
  dry_run: boolean;
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
  events?: Array<{
    path: string;
    kind: "added" | "changed" | "deleted" | "unsupported";
    old_sha256?: string;
    sha256?: string;
    reason?: string;
  }>;
  unsupported?: Array<{
    path: string;
    reason: string;
  }>;
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

export interface ChatMessageRecord {
  id: string;
  role: "user" | "assistant";
  content: string;
  answer?: QueryAnswer;
  writeback?: QueryWriteback;
  created_at: string;
}

export interface ChatSessionRecord {
  id: string;
  project_path: string;
  title: string;
  messages: ChatMessageRecord[];
  created_at: string;
  updated_at: string;
}

export interface ChatsResponse {
  sessions: ChatSessionRecord[];
  count: number;
}

export interface ChatResponse {
  session: ChatSessionRecord;
}

export interface ChatAppendResponse {
  session: ChatSessionRecord;
  answer: QueryAnswer;
  writeback?: QueryWriteback;
}

export interface ChatRun {
  id: string;
  chat_id: string;
  project_path: string;
  project_id: string;
  status: "running" | "succeeded" | "failed" | "canceled";
  question: string;
  started_at: string;
  finished_at?: string;
  error?: string;
}

export interface ChatRunEvent {
  id: string;
  run_id: string;
  type: string;
  time: string;
  step?: number;
  action?: {
    action: string;
    path?: string;
    query?: string;
    limit?: number;
    title?: string;
    answer?: string;
    rationale?: string;
  };
  message: string;
  observation?: string;
  elapsed_ms: number;
  session?: ChatSessionRecord;
  answer?: QueryAnswer;
  writeback?: QueryWriteback;
}

export interface ChatRunResponse {
  run: ChatRun;
  session: ChatSessionRecord;
}

export interface ReviewItem {
  ID: string;
  ProjectID: string;
  Type: string;
  Title: string;
  Description: string;
  Severity: string;
  Status: string;
  SourcePath: string;
  AffectedPages: string[];
  SearchQueries: string[];
  Options: ReviewOption[];
  ResolvedAction: string;
  CreatedAt: string;
  ResolvedAt: string | null;
}

export interface ReviewOption {
  Label: string;
  Action: string;
}

export interface ReviewsResponse {
  reviews: ReviewItem[];
  count: number;
}

export interface ReviewActionResponse {
  review: ReviewItem;
  written_paths: string[];
  message: string;
}

export interface ReviewSweepResponse {
  rule_resolved: number;
  llm_resolved: number;
  reviews: ReviewItem[];
}

export interface ResearchJob {
  id: string;
  project_path: string;
  project_id: string;
  status: "queued" | "running" | "succeeded" | "failed";
  request: {
    project_path?: string;
    project_id?: string;
    review_id?: string;
    topic?: string;
    query?: string;
    agent?: AgentName;
  };
  written_paths?: string[];
  review?: ReviewItem;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface ResearchJobResponse {
  job: ResearchJob;
  existing?: boolean;
}

export interface ResearchJobsResponse {
  jobs: ResearchJob[];
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
