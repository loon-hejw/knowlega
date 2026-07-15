import { lazy } from "react";
import { Navigate, Route, Routes, useNavigate, useSearchParams } from "react-router-dom";
import { browserRoutes } from "./navigation";
import { useWorkspace } from "./WorkspaceContext";
import { WorkspaceLayout, useWorkspaceOutlet } from "../layouts/WorkspaceLayout";
const BrowserHomePage = lazy(() => import("../pages/browse/HomePage").then((module) => ({ default: module.BrowserHomePage })));
const KnowledgeDiscoveryPage = lazy(() => import("../pages/browse/ExplorePages").then((module) => ({ default: module.KnowledgeDiscoveryPage })));
const CodeKnowledgePage = lazy(() => import("../pages/browse/ExplorePages").then((module) => ({ default: module.CodeKnowledgePage })));
const CollectionsPage = lazy(() => import("../pages/browse/ExplorePages").then((module) => ({ default: module.CollectionsPage })));
const SourceLibraryPage = lazy(() => import("../pages/browse/ExplorePages").then((module) => ({ default: module.SourceLibraryPage })));
const BrowseGraphPage = lazy(() => import("../pages/browse/ExplorePages").then((module) => ({ default: module.BrowseGraphPage })));
const QueryPage = lazy(() => import("../features/query/QueryPage").then((module) => ({ default: module.QueryPage })));
const WorkbenchPage = lazy(() => import("../features/wiki/WorkbenchPage").then((module) => ({ default: module.WorkbenchPage })));
const Dashboard = lazy(() => import("../pages/admin/DashboardPage").then((module) => ({ default: module.Dashboard })));
const KnowledgePage = lazy(() => import("../pages/admin/KnowledgeAdminPage").then((module) => ({ default: module.KnowledgePage })));
const ReviewsPage = lazy(() => import("../pages/admin/ReviewsPage").then((module) => ({ default: module.ReviewsPage })));
const SourcesPage = lazy(() => import("../pages/admin/SourcesPage").then((module) => ({ default: module.SourcesPage })));
const WikiOpsPage = lazy(() => import("../pages/admin/WikiAdminPage").then((module) => ({ default: module.WikiOpsPage })));
const loadAdminSupport = () => import("../pages/admin/AdminSupportPages");
const RepositoriesPage = lazy(() => loadAdminSupport().then((module) => ({ default: module.RepositoriesPage })));
const AdminJobsPage = lazy(() => loadAdminSupport().then((module) => ({ default: module.AdminJobsPage })));
const QualityPage = lazy(() => loadAdminSupport().then((module) => ({ default: module.QualityPage })));
const SystemDiagnosticsPage = lazy(() => loadAdminSupport().then((module) => ({ default: module.SystemDiagnosticsPage })));

export function AppRoutes() {
  return (
    <Routes>
      <Route element={<WorkspaceLayout />}>
        <Route index element={<HomeRoute />} />
        <Route path="query" element={<QueryRoute />} />
        <Route path="topics" element={<TopicsRoute />} />
        <Route path="code" element={<CodeRoute />} />
        <Route path="collections" element={<CollectionsRoute />} />
        <Route path="wiki" element={<WikiRoute />} />
        <Route path="sources" element={<SourcesLibraryRoute />} />
        <Route path="graph" element={<BrowseGraphRoute />} />

        <Route path="admin" element={<AdminOverviewRoute />} />
        <Route path="admin/sources" element={<AdminSourcesRoute />} />
        <Route path="admin/wiki" element={<AdminWikiRoute />} />
        <Route path="admin/repositories" element={<RepositoriesRoute />} />
        <Route path="admin/reviews" element={<ReviewsRoute />} />
        <Route path="admin/jobs" element={<JobsRoute />} />
        <Route path="admin/quality" element={<QualityRoute />} />
        <Route path="admin/settings" element={<SettingsRoute />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

function HomeRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  const navigate = useNavigate();
  return (
    <BrowserHomePage
      api={api}
      settings={settings}
      onNavigate={(page) => navigate(browserRoutes[page])}
      onOpenFile={openFile}
      onAsk={(question) => navigate(`/query?q=${encodeURIComponent(question)}`)}
      onTopic={(query) => navigate(`/topics?q=${encodeURIComponent(query)}`)}
    />
  );
}

function QueryRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  const [searchParams] = useSearchParams();
  return <QueryPage api={api} settings={settings} onOpenFile={openFile} initialQuestion={searchParams.get("q") ?? ""} />;
}

function TopicsRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  const [searchParams] = useSearchParams();
  return <KnowledgeDiscoveryPage api={api} settings={settings} onOpenFile={openFile} initialQuery={searchParams.get("q") ?? ""} />;
}

function CodeRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  return <CodeKnowledgePage api={api} settings={settings} onOpenFile={openFile} />;
}

function CollectionsRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  return <CollectionsPage api={api} settings={settings} onOpenFile={openFile} />;
}

function WikiRoute() {
  const { api, settings } = useWorkspace();
  const [searchParams] = useSearchParams();
  return <WorkbenchPage api={api} settings={settings} initialPath={searchParams.get("path") ?? "wiki/index.md"} readOnly />;
}

function SourcesLibraryRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  return <SourceLibraryPage api={api} settings={settings} onOpenFile={openFile} />;
}

function BrowseGraphRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  return <BrowseGraphPage api={api} settings={settings} onOpenFile={openFile} />;
}

function AdminOverviewRoute() {
  const { api, settings } = useWorkspace();
  return <Dashboard api={api} settings={settings} />;
}

function AdminSourcesRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  return <KnowledgePage api={api} settings={settings} onOpenFile={openFile} />;
}

function AdminWikiRoute() {
  const { api, settings } = useWorkspace();
  return <WikiOpsPage api={api} settings={settings} />;
}

function RepositoriesRoute() {
  const { api, settings } = useWorkspace();
  return <RepositoriesPage api={api} settings={settings} />;
}

function ReviewsRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  return <ReviewsPage api={api} settings={settings} onOpenFile={openFile} />;
}

function JobsRoute() {
  const { api, settings } = useWorkspace();
  return <AdminJobsPage api={api} settings={settings} />;
}

function QualityRoute() {
  const { api, settings } = useWorkspace();
  const { openFile } = useWorkspaceOutlet();
  return <QualityPage api={api} settings={settings} onOpenFile={openFile} />;
}

function SettingsRoute() {
  const { api, settings, updateSettings } = useWorkspace();
  return <SystemDiagnosticsPage api={api} settings={settings} updateSettings={updateSettings} />;
}
