import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { ApiClient } from "../api/client";
import type { AppSettings, WorkspaceStatus } from "../types/api";
import { errorMessage } from "../shared/utils";

export type StartupState =
  | { kind: "loading" }
  | { kind: "bootstrapping"; status: WorkspaceStatus }
  | { kind: "ready"; status: WorkspaceStatus }
  | { kind: "error"; message: string };

type WorkspaceContextValue = {
  api: ApiClient;
  settings: AppSettings;
  startup: StartupState;
  reloadWorkspace: () => Promise<void>;
  updateSettings: (patch: Partial<AppSettings>) => void;
};

const defaultSettings: AppSettings = {
  apiBaseUrl: "/api",
  projectPath: "",
  projectID: "",
  agent: "llm",
};

const WorkspaceContext = createContext<WorkspaceContextValue | null>(null);

export function WorkspaceProvider({ children }: { children: ReactNode }) {
  const [settings, setSettings] = useState<AppSettings>(defaultSettings);
  const [startup, setStartup] = useState<StartupState>({ kind: "loading" });
  const api = useMemo(() => new ApiClient(settings.apiBaseUrl), [settings.apiBaseUrl]);

  const updateSettings = useCallback((patch: Partial<AppSettings>) => {
    setSettings((current) => ({ ...current, ...patch }));
  }, []);

  const reloadWorkspace = useCallback(async () => {
    try {
      const status = await api.workspaceStatus({});
      setSettings((current) => ({
        ...current,
        projectPath: status.project_path,
        projectID: status.project_id,
        agent: "llm",
      }));
      if (status.bootstrap && status.bootstrap.status !== "succeeded") {
        if (status.bootstrap.status === "failed") {
          throw new Error(status.bootstrap.error || "项目初始化失败");
        }
        setStartup({ kind: "bootstrapping", status });
        return;
      }
      if (!status.ok) {
        const missing = status.files.filter((file) => !file.exists).map((file) => file.path);
        const detail = status.lint.error || (missing.length > 0 ? `缺少文件：${missing.join("、")}` : "项目状态不可用");
        throw new Error(detail);
      }
      setStartup({ kind: "ready", status });
    } catch (error) {
      setStartup({ kind: "error", message: errorMessage(error) });
    }
  }, [api]);

  useEffect(() => {
    void reloadWorkspace();
  }, [reloadWorkspace]);

  useEffect(() => {
    if (startup.kind !== "error" && startup.kind !== "bootstrapping") return;
    const delay = startup.kind === "bootstrapping" ? 1000 : 2500;
    const timer = window.setTimeout(() => void reloadWorkspace(), delay);
    return () => window.clearTimeout(timer);
  }, [reloadWorkspace, startup]);

  const value = useMemo(
    () => ({ api, settings, startup, reloadWorkspace, updateSettings }),
    [api, reloadWorkspace, settings, startup, updateSettings],
  );

  return <WorkspaceContext.Provider value={value}>{children}</WorkspaceContext.Provider>;
}

export function useWorkspace(): WorkspaceContextValue {
  const value = useContext(WorkspaceContext);
  if (!value) throw new Error("useWorkspace must be used inside WorkspaceProvider");
  return value;
}
