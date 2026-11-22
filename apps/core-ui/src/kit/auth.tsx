import { useEffect, useMemo, useState, type ReactNode } from "react";
import { getAuthApiBase } from "./api";
import {
  clearStoredSession,
  readStoredSession,
  redirectToLogin,
  type SsoSession,
} from "./sso";
import { isTokenExpired, refreshSession } from "./auth/tokenStore";

// ---------------------------------------------------------------------------
// 统一认证（apps/auth :8008）接入层 — 非枢纽 board 服务版（无 web3auth SDK）
//
// 统一登录设计：web3auth 登录框只在枢纽（board-ops :5181）/login 页弹，
// 其他服务一律不能弹 modal。本层只关心登录态：
//  - 会话读自 localStorage "web3auth:session"（枢纽登录后经 ?sso= 回跳写入，
//    见 sso.ts consumeSsoOnBoot）
//  - login() = 跳转枢纽 /login 获取凭证（redirectToLogin，不弹框）
//  - logout() = 调 auth /logout + 清本地会话
//  - 401 → api.ts 先用本 origin 注册的全局 handler 静默续期并重放；仍 401 → auth:required
//    → SsoGate 跳枢纽 /login
//  - 本层与枢纽同 origin（core-ui 单容器）：refresh token 只此一份，轮换不会触发
//    跨 origin 重放检测；跨 origin 服务改用 /ops/sso iframe 由枢纽代续。
// ---------------------------------------------------------------------------

import { AuthContext, type AuthContextValue, type AuthUserFromSession } from "./auth/context";

function AuthInner({ children }: { children: ReactNode }) {
  // 会话在渲染前已由 consumeSsoOnBoot() 写入；登出时本地清除
  const [ready, setReady] = useState(false);
  const [session, setSession] = useState<SsoSession | null>(null);
  useEffect(() => {
    setSession(readStoredSession());
    setReady(true);
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

  const token = () => session?.token ?? "";

  // 角色：会话 userInfo 不含 roles，登录后调 auth /me 实时拉取
  //（管理页门控用；拉取失败不阻塞登录态，roles 为空）。
  const [roles, setRoles] = useState<string[]>([]);
  const isConnected = Boolean(session?.token && session.address);
  useEffect(() => {
    if (!isConnected) {
      setRoles([]);
      return;
    }
    let cancelled = false;
    const t = token();
    if (!t) return;
    (async () => {
      try {
        const res = await fetch(`${getAuthApiBase()}/me`, {
          headers: { Authorization: `Bearer ${t}` },
        });
        if (!res.ok) return;
        const data = await res.json();
        if (!cancelled) setRoles(Array.isArray(data?.roles) ? data.roles : []);
      } catch {
        /* 角色拉取失败不影响登录态 */
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isConnected]);

  // 同源静默续期：refresh token 在本 origin 只有一份（枢纽单容器），
  // 轮换不会触发 auth 的跨 origin 重放检测。跨 origin 服务另走 /ops/sso iframe。
  const refresh = async (): Promise<string | null> => {
    const s = readStoredSession();
    if (!s) return null;
    if (!isTokenExpired(s.token)) return s.token;
    const fresh = await refreshSession();
    return fresh?.token ?? null;
  };

  const logout = async () => {
    const t = token();
    try {
      if (t) {
        await fetch(`${getAuthApiBase()}/logout`, {
          method: "POST",
          headers: { "Content-Type": "application/json", Authorization: `Bearer ${t}` },
          body: "{}",
        });
      }
    } catch {
      /* 本地登出不受服务端影响 */
    }
    clearStoredSession();
    setSession(null);
  };

  const value = useMemo<AuthContextValue>(
    () => ({
      isInitialized: ready,
      isConnected,
      isConnecting: false,
      connectError: null,
      user: isConnected
        ? {
            id:
              (session?.userInfo as AuthUserFromSession | undefined)?.verifierId ||
              (session?.userInfo as AuthUserFromSession | undefined)?.email ||
              "",
            email: (session?.userInfo as AuthUserFromSession | undefined)?.email,
            name: (session?.userInfo as AuthUserFromSession | undefined)?.name,
            roles,
          }
        : null,
      login: () => {
        redirectToLogin();
        return Promise.resolve();
      },
      logout,
      token,
      refresh,
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [ready, isConnected, session, roles],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  return <AuthInner>{children}</AuthInner>;
}


