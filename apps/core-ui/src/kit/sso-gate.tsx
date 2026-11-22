import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  bumpSsoLoop,
  clearSsoLoop,
  isHubLoginPage,
  readStoredSession,
  redirectToLogin,
  type SsoSession,
} from "./sso";
import { isTokenExpired, refreshSession } from "./auth/tokenStore";

/**
 * SsoGate：多服务登录跳转门（挂在各服务根 provider 树内，与枢纽/pi-web 的 SDK 版语义一致）：
 *  - 未登录（localStorage 无 "web3auth:session" 会话）/ token 已过期
 *    → 先尝试同源静默续期（枢纽 origin 唯一合法轮换者），失败再跳枢纽 /login（?sso_next=原地址）
 *  - 已在枢纽 /login 页 → 不跳（那页自己弹登录框）
 *  - 401（auth:required 事件，api.ts fetch 层派发，已节流）→ 同样由 api 层先续期重放，
 *    仍 401 才跳 /login
 *  - 有有效会话 → 清零循环计数器；跨加载循环计数器 60s 内 >3 次 → 停止跳转（防死循环）
 *
 * 不依赖 web3auth SDK context（读 localStorage 会话即可）。
 * （?sso_next= 登录成功后的回跳由枢纽 /login 页处理）
 * （跨 origin 服务读不到枢纽 localStorage → 走 /ops/sso iframe，见 apps/pi-web/lib/sso.ts）
 */
export function SsoGate({ children }: { children: ReactNode }) {
  const [ready, setReady] = useState(false);
  const [session, setSession] = useState<SsoSession | null>(null);
  const redirected = useRef(false);

  // 客户端挂载后读一次会话（渲染前 consumeSsoOnBoot 已把 ?sso= 写入 localStorage）；
  // token 已过期 → 同源静默续期一次（枢纽 origin 唯一合法轮换者）再决定是否跳登录。
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const s = readStoredSession();
      if (s && isTokenExpired(s.token)) {
        const fresh = await refreshSession();
        if (!cancelled) setSession(fresh ?? null);
      } else if (!cancelled) {
        setSession(s);
      }
      if (!cancelled) setReady(true);
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // 其他标签页登录 / 续期 / 登出 → 同步本页内存态（localStorage 同源共享）
  useEffect(() => {
    const onStorage = (e: StorageEvent) => {
      if (e.key === null || e.key === "web3auth:session" || e.key === "web3auth:user") {
        setSession(readStoredSession());
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  useEffect(() => {
    if (!ready || redirected.current) return;
    if (session) {
      clearSsoLoop();
      return;
    }
    if (isHubLoginPage()) return;
    if (bumpSsoLoop()) return;
    redirected.current = true;
    redirectToLogin();
  }, [ready, session]);

  // 401（无 token / 过期 / 会话被吊销）→ 去枢纽 /login 换新凭证
  useEffect(() => {
    const handler = () => {
      if (isHubLoginPage()) return;
      redirectToLogin();
    };
    window.addEventListener("auth:required", handler);
    return () => window.removeEventListener("auth:required", handler);
  }, []);

  return <>{children}</>;
}
