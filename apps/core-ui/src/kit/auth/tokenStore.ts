/**
 * Token 存储 / 刷新逻辑（枢纽 core-ui :5181 专用）。
 *
 * 与 sso.ts 的 readStoredSession / clearStoredSession 共用同一份
 * localStorage "web3auth:session"，额外提供 JWT exp 解析、临期判断和 refresh 轮换。
 *
 * ⚠️ 仅枢纽 origin 允许调用 refreshSession()：
 * refresh token 经 ?sso= 复制到各 origin，谁先轮换谁的旧拷贝就是
 * "重放"→ auth 的轮换+重放检测会吊销全部会话。
 * 使用方（都在枢纽 origin）：
 *  - main.tsx：全域 401 静默续期 handler（setRefreshHandler）
 *  - kit/sso-frame.tsx：/ops/sso iframe 端点（代跨 origin 服务续期）
 *  - kit/sso-gate.tsx / kit/auth.tsx：同 origin 分区会话临期续期
 */

import { getAuthApiBase } from "../api";
import type { SsoSession } from "../sso";

const SESSION_KEY = "web3auth:session";

// ---------------------------------------------------------------------------
// 读写
// ---------------------------------------------------------------------------

export function readSession(): SsoSession | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = localStorage.getItem(SESSION_KEY);
    if (!raw) return null;
    const s = JSON.parse(raw) as SsoSession;
    if (!s?.token || !s?.address) return null;
    return s;
  } catch {
    return null;
  }
}

export function writeSession(s: SsoSession): void {
  if (typeof window === "undefined") return;
  localStorage.setItem(SESSION_KEY, JSON.stringify(s));
}

export function clearSession(): void {
  if (typeof window === "undefined") return;
  localStorage.removeItem(SESSION_KEY);
  localStorage.removeItem("web3auth:user");
}

// ---------------------------------------------------------------------------
// JWT exp 解析
// ---------------------------------------------------------------------------

/** 解析 JWT exp（秒）；解析失败返回 null（交给服务端校验） */
export function parseTokenExp(token: string): number | null {
  try {
    const payload = token.split(".")[1];
    if (!payload) return null;
    const b64 =
      payload.replace(/-/g, "+").replace(/_/g, "/") +
      "=".repeat((4 - (payload.length % 4)) % 4);
    const json = JSON.parse(decodeURIComponent(escape(atob(b64)))) as {
      exp?: number;
    };
    return typeof json.exp === "number" ? json.exp : null;
  } catch {
    return null;
  }
}

/**
 * 判断 token 是否已过期（含 bufferMs 缓冲，默认 30 s）。
 * 解析失败视为未过期（交给服务端 / 下游 401 处理）。
 */
export function isTokenExpired(token: string, bufferMs = 30_000): boolean {
  const exp = parseTokenExp(token);
  if (exp === null) return false;
  return exp * 1000 < Date.now() + bufferMs;
}

/** token 是否在 nearMs 内过期（默认 5 min），用于 iframe 端点提前续期 */
export function isTokenNearExpiry(token: string, nearMs = 5 * 60_000): boolean {
  const exp = parseTokenExp(token);
  if (exp === null) return false;
  return exp * 1000 < Date.now() + nearMs;
}

// ---------------------------------------------------------------------------
// Refresh token 轮换（仅枢纽）
// ---------------------------------------------------------------------------

let refreshInFlight: Promise<SsoSession | null> | null = null;

/**
 * 用 refresh token 轮换出新 access token 并写回 localStorage。
 * 全局单例锁：并发调用只发一次 refresh。
 * 成功返回新会话，失败返回 null。
 */
export function refreshSession(): Promise<SsoSession | null> {
  refreshInFlight =
    refreshInFlight ??
    doRefresh().finally(() => {
      refreshInFlight = null;
    });
  return refreshInFlight;
}

async function doRefresh(): Promise<SsoSession | null> {
  const session = readSession();
  if (!session?.refreshToken) return null;
  try {
    const res = await fetch(`${getAuthApiBase()}/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: session.refreshToken }),
    });
    if (!res.ok) return null;
    const data = await res.json();
    if (!data?.access_token) return null;
    const newSession: SsoSession = {
      ...session,
      token: data.access_token,
      refreshToken: data.refresh_token ?? session.refreshToken,
    };
    writeSession(newSession);
    return newSession;
  } catch {
    return null;
  }
}

/**
 * 验证当前会话：token 有效 → 直接返回；已过期 → 尝试 refresh。
 * 返回有效会话或 null（彻底过期 / 无会话）。
 */
export async function verifyAndRefresh(): Promise<SsoSession | null> {
  const session = readSession();
  if (!session) return null;
  if (!isTokenExpired(session.token)) return session;
  return refreshSession();
}
