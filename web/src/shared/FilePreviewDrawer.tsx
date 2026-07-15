import { Alert, App as AntApp, Descriptions, Drawer, Empty, Space, Typography } from "antd";
import { useEffect, useState } from "react";
import { ApiClient } from "../api/client";
import type { AppSettings, ProjectFileContent } from "../types/api";
import { errorMessage, formatDateTime, renderMarkdown } from "./utils";

const { Text } = Typography;

export function FilePreviewDrawer({
  api,
  settings,
  path,
  onClose,
  onOpenFile,
}: {
  api: ApiClient;
  settings: AppSettings;
  path: string | null;
  onClose: () => void;
  onOpenFile: (path: string) => void;
}) {
  const { message } = AntApp.useApp();
  const [content, setContent] = useState<ProjectFileContent | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!path) {
      setContent(null);
      return;
    }
    setLoading(true);
    api
      .projectFileContent(settings.projectPath, path)
      .then(setContent)
      .catch((error) => message.error(errorMessage(error)))
      .finally(() => setLoading(false));
  }, [api, message, path, settings.projectPath]);

  return (
    <Drawer width={760} title={path || "页面预览"} open={Boolean(path)} onClose={onClose} extra={path ? <Text code copyable={{ text: path }}>{path}</Text> : null}>
      {loading ? (
        <Alert type="info" showIcon message="正在加载页面" />
      ) : content ? (
        <Space direction="vertical" size={12} className="page-stack">
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label="标题">{content.title || "-"}</Descriptions.Item>
            <Descriptions.Item label="类型">{content.type || content.kind || "-"}</Descriptions.Item>
            <Descriptions.Item label="更新时间">{formatDateTime(content.mod_time)}</Descriptions.Item>
          </Descriptions>
          <div className="markdown-view">{renderMarkdown(content.content, onOpenFile)}</div>
        </Space>
      ) : (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无内容" />
      )}
    </Drawer>
  );
}
