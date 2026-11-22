// ---------------------------------------------------------------------------
// AI 后端（2026-11-20：ai-flow = 唯一后端）
// 旧 board_ai 链路（workflow-api /boards/:id/ai-chat/stream）已删除，board 对话
// 流式端点固定为 ai-flow 网关：${origin}/api/ai-flow/chat（core-ui nginx 反代
// → ai-flow:8134，proxy_buffering off 保 SSE）。
// SSE 事件协议：token / tool / tool_result / result / error；result 帧携带
// tool_calls（变更提案），由前端确认后走 /boards/:id/ai-execute 落卡。
// ---------------------------------------------------------------------------

export const AI_FLOW_CHAT_ENDPOINT = "/api/ai-flow/chat";

// streamEndpoint：AI 对话流式请求的完整 URL。端点与看板 id 无关（ai-flow 侧从请求体
// board_id/session 定位），故无入参。
export function streamEndpoint(): string {
    return `${window.location.origin}${AI_FLOW_CHAT_ENDPOINT}`;
}
