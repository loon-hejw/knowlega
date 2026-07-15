import type { ThemeConfig } from "antd";

export const appTheme: ThemeConfig = {
  token: {
    colorPrimary: "#0f7f78",
    colorText: "#122747",
    colorTextSecondary: "#64748b",
    colorBgLayout: "#f7f8fa",
    colorBorder: "#e4e9ef",
    borderRadius: 10,
    fontSize: 15,
    controlHeight: 40,
  },
  components: {
    Button: { primaryShadow: "none" },
    Layout: { bodyBg: "#f7f8fa", headerBg: "#ffffff", siderBg: "#ffffff" },
    Menu: {
      itemBorderRadius: 8,
      itemHeight: 44,
      itemSelectedBg: "#edf7f5",
      itemSelectedColor: "#0f7f78",
    },
    Card: {
      borderRadiusLG: 14,
    },
  },
};
