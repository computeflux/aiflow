import { useEffect } from "react";
import {
  isTokenExpired,
  isTokenNearExpiry,
  readSession,
  refreshSession,
} from "./auth/tokenStore";
import type { SsoSession } from "./sso";

// ---------------------------------------------------------------------------
// iframe 静默续期 / 会话同步端点（枢纽 origin，路由 /ops/sso）
//
// 非枢纽服务（pi-web 等，独立 origin）读不到枢纽的 localStorage，于是在自己页面里
// 内嵌一个隐藏 iframe 指向本页，postMessage {type:"sso:sync", nonce}；
// 本页用**枢纽唯一的 refresh token** 续期后回传 {type:"sso:session", nonce, session}。
//
// 为什么必须由枢纽代续：refresh token 经 ?sso= 复制到各 origin 会触发 auth 的
// 轮换+重放检测（refresh_reuse_detected → 吊销全部会话）。轮换只允许发生在枢纽 origin。
//
// 红线：
//  - 只回传给「同 origin / 同 hostname / VITE_SSO_ALLOWED_ORIGINS」白名单来源，
//    targetOrigin 精确指定（绝不用 "*"）；
//  - 只回传会话字段（token/address/userInfo），不读不传其他 localStorage。
// ---------------------------------------------------------------------------

const SSO_SYNC_REQUEST = "sso:sync";
const SSO_SYNC_RESPONSE = "sso:session";

/** 额外允许的 origin（逗号分隔）；同 origin / 同 hostname 默认放行 */
function envAllowedOrigins(): string[] {
  const raw = (import.meta.env.VITE_SSO_ALLOWED_ORIGINS as string | undefined) ?? "";
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

function isAllowedOrigin(origin: string): boolean {
  if (typeof window === "undefined" || !origin) return false;
  if (origin === window.location.origin) return true;
  try {
    // 同 hostname 任意端口（pi-web :8443、contract-deploy :8000 等，与枢纽同 site，
    // 第三方存储分区也按 site 划分 → 同 site iframe 能读到枢纽未分区的 localStorage）
    if (new URL(origin).hostname === window.location.hostname) return true;
  } catch {
    return false;
  }
  return envAllowedOrigins().includes(origin);
}

/**
 * SsoFrame：空白页，只做 postMessage 应答（不挂 SsoGate，避免无会话时被重定向）。
 * 挂载在 main.tsx 的 /ops/sso 路由（在 /ops/* 之外）。
 */
export function SsoFrame() {
  useEffect(() => {
    const onMessage = (ev: MessageEvent) => {
      const data = ev.data as { type?: string; nonce?: string } | null;
      if (!data || data.type !== SSO_SYNC_REQUEST || !data.nonce) return;
      if (!isAllowedOrigin(ev.origin)) return; // 静默拒绝（不回错误，避免被探测）
      const source = ev.source as Window | null;
      if (!source) return;

      void (async () => {
        let session: SsoSession | null = readSession();
        // 临期（默认 5 分钟）→ 用枢纽 refresh token 续期；续期失败但旧 token 仍有效则继续用
        if (session && isTokenNearExpiry(session.token)) {
          const fresh = await refreshSession();
          if (fresh) {
            session = fresh;
          } else if (isTokenExpired(session.token)) {
            session = null;
          }
        }
        source.postMessage(
          { type: SSO_SYNC_RESPONSE, nonce: data.nonce, session },
          ev.origin,
        );
      })();
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, []);

  return null;
}
