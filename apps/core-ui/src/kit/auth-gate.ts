// auth 事件桥：登出/401 后跳转登录提示（简单事件，非组件，供 api 层与页面订阅）
// 登出后跳转登录提示（简单事件）
export function onAuthRequired(fn: () => void): () => void {
  const handler = () => fn();
  window.addEventListener("auth:required", handler);
  return () => window.removeEventListener("auth:required", handler);
}
export function emitAuthRequired(): void {
  window.dispatchEvent(new Event("auth:required"));
}
