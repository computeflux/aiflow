// board-kit：board 拆分为多独立服务后的共享包（file: 源码依赖，同 web3auth 模式）。
// 各服务（board-ops / board-content / board-email / board-product）引用本包，
// 避免 api/auth/Sidebar/组件四份拷贝。
export * from "./api";
export * from "./auth";
export * from "./sso";
export * from "./sso-gate";
export * from "./utils";
export * from "./hooks/useAsync";
export * from "./components/Sidebar";
export * from "./components/ErrorBoundary";
export * from "./components/ConfirmModal";
export * from "./components/Pagination";
export * from "./components/StatsHeader";
export * from "./components/AIPanel";
export * from "./components/AISidebar";
export * from "./components/RichEditor";
export * from "./auth-gate";
export * from "./ai-backend";
export * from "./hooks/useAuth";
