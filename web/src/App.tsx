import { App as AntApp, ConfigProvider } from "antd";
import { BrowserRouter } from "react-router-dom";
import { AppRoutes } from "./app/router";
import { appTheme } from "./app/theme";
import { WorkspaceProvider } from "./app/WorkspaceContext";

export default function App() {
  return (
    <ConfigProvider theme={appTheme}>
      <AntApp>
        <BrowserRouter>
          <WorkspaceProvider>
            <AppRoutes />
          </WorkspaceProvider>
        </BrowserRouter>
      </AntApp>
    </ConfigProvider>
  );
}
