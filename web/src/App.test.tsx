import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App";

const activeStatus = {
  ok: true,
  service: "knowledge-core",
  time: "2026-07-10T00:00:00Z",
  project_path: "/private/tmp/kbcore-xiyouji-wiki",
  project_id: "local",
  agent: "llm",
  files: [
    { path: "purpose.md", exists: true },
    { path: "schema.md", exists: true },
    { path: "wiki/index.md", exists: true },
    { path: "wiki/overview.md", exists: true },
    { path: "wiki/reviews.md", exists: true },
  ],
  queue: { pending: 0, processing: 0, done: 0, failed: 0, total: 0 },
  reviews: { open: 0, resolved: 0, dismissed: 0, total: 0 },
  lint: { count: 0 },
  pg_configured: false,
  embedding_configured: false,
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.replaceState({}, "", "/");
});

describe("application startup", () => {
  it("shows an explicit loading state while the backend status is pending", () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => undefined)));
    render(<App />);
    expect(screen.getByText("正在读取后端配置并加载活动项目…")).toBeInTheDocument();
  });

  it("loads and selects the configured active project", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => responseFor(input)));
    render(<App />);
    expect(await screen.findByRole("heading", { name: "继续你的研究" })).toBeInTheDocument();
    expect(screen.getByText("知识库可用")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /管理/ }));
    expect((await screen.findAllByText("/private/tmp/kbcore-xiyouji-wiki")).length).toBeGreaterThan(0);
  });

  it("exposes separate browse and admin workspaces", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => responseFor(input)));
    render(<App />);
    await screen.findByRole("heading", { name: "继续你的研究" });
    for (const menu of ["搜索与提问", "Wiki 知识库", "主题与实体", "系统与代码", "知识集合", "知识图谱"]) {
      expect(screen.getByRole("menuitem", { name: new RegExp(`${menu}$`) })).toBeInTheDocument();
    }
    fireEvent.click(screen.getByRole("button", { name: /管理/ }));
    for (const menu of ["统一来源库", "Wiki 治理", "代码仓库", "审阅中心", "任务中心", "质量治理", "系统诊断"]) {
      expect(await screen.findByRole("menuitem", { name: new RegExp(`${menu}$`) })).toBeInTheDocument();
    }
    fireEvent.click(screen.getByRole("menuitem", { name: /任务中心$/ }));
    expect(await screen.findByRole("heading", { name: "任务中心", level: 2 })).toBeInTheDocument();
  });

  it("shows background bootstrap source and stage progress", async () => {
    const bootstrapping = {
      ...activeStatus,
      ok: false,
      bootstrap: {
        status: "running",
        stage: "generation",
        project_path: activeStatus.project_path,
        current_source: "/sources/chapter-041.txt",
        current_source_num: 41,
        completed_sources: 40,
        total_sources: 100,
        files: 120,
        reviews: 40,
        attempt: 1,
        started_at: "2026-07-10T00:00:00Z",
        updated_at: "2026-07-10T00:01:00Z",
      },
    };
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(bootstrapping)));
    render(<App />);
    expect(await screen.findByText("正在构建知识库")).toBeInTheDocument();
    expect(screen.getByText("40 / 100 个来源已完成")).toBeInTheDocument();
    expect(screen.getByText("阶段：生成 Wiki 页面")).toBeInTheDocument();
    expect(screen.getByText(/chapter-041.txt/)).toBeInTheDocument();
  });

  it("renders a visible error and recovers through retry", async () => {
    let failed = true;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (failed) {
          failed = false;
          throw new Error("backend unavailable");
        }
        return responseFor(input);
      }),
    );
    render(<App />);
    expect(await screen.findByText("无法加载活动项目")).toBeInTheDocument();
    expect(screen.getByText("backend unavailable")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /重试/ }));
    await waitFor(() => expect(screen.getByRole("heading", { name: "继续你的研究" })).toBeInTheDocument());
  });
});

function responseFor(input: RequestInfo | URL): Response {
  const url = new URL(String(input), window.location.origin);
  let body: unknown = {};
  if (url.pathname.endsWith("/workspace/status")) body = activeStatus;
  if (url.pathname.endsWith("/workspace/jobs")) body = { jobs: [], count: 0 };
  if (url.pathname.endsWith("/queue/tasks")) body = { version: 1, tasks: [] };
  if (url.pathname.endsWith("/projects/sources")) body = { sources: [], count: 0 };
  if (url.pathname.endsWith("/projects/files")) body = { files: [], count: 0 };
  if (url.pathname.endsWith("/projects/files/content")) body = { path: "wiki/index.md", kind: "wiki", content: "# Index", sources: [] };
  if (url.pathname.endsWith("/projects/graph")) body = { nodes: [], edges: [] };
  if (url.pathname.endsWith("/projects/graph/insights")) body = { isolated_pages: [], missing_sources: [], bridge_candidates: [], hub_pages: [] };
  if (url.pathname.endsWith("/reviews")) body = { reviews: [], count: 0 };
  if (url.pathname.endsWith("/research/jobs")) body = { jobs: [], count: 0 };
  if (url.pathname.endsWith("/chats")) body = { sessions: [], count: 0 };
  return jsonResponse(body);
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}
