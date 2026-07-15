export type BrowserPageKey =
  | "overview"
  | "query"
  | "wiki"
  | "sources"
  | "topics"
  | "code"
  | "collections"
  | "graph";

export type AdminPageKey =
  | "overview"
  | "sources"
  | "wiki"
  | "repositories"
  | "reviews"
  | "jobs"
  | "quality"
  | "settings";

export type SurfaceMode = "browse" | "admin";

export const browserRoutes: Record<BrowserPageKey, string> = {
  overview: "/",
  query: "/query",
  wiki: "/wiki",
  sources: "/sources",
  topics: "/topics",
  code: "/code",
  collections: "/collections",
  graph: "/graph",
};

export const adminRoutes: Record<AdminPageKey, string> = {
  overview: "/admin",
  sources: "/admin/sources",
  wiki: "/admin/wiki",
  repositories: "/admin/repositories",
  reviews: "/admin/reviews",
  jobs: "/admin/jobs",
  quality: "/admin/quality",
  settings: "/admin/settings",
};

export function surfaceFromPath(pathname: string): SurfaceMode {
  return pathname === "/admin" || pathname.startsWith("/admin/") ? "admin" : "browse";
}
