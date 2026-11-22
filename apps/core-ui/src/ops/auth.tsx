import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Web3AuthProvider,
  useWeb3Auth,
  useWeb3AuthConnect,
  useWeb3AuthDisconnect,
  useWeb3AuthUser,
  type Web3AuthContextConfig,
  SsoGate,
} from "@wetee/web3auth/react";
import { getAuthApiBase, setRefreshHandler } from "./api";

// ---------------------------------------------------------------------------
// 枢纽专属：refresh token 轮换（唯一合法轮换者）
//
// refresh token 经 ?sso= 复制到各 origin 各持一份拷贝，谁先轮换谁的旧拷贝就是
// "重放"→ auth 的轮换+重放检测会吊销全部会话（auth_audit_logs: refresh_reuse_detected）。
// 因此仅枢纽（本服务）允许轮换：
//  - 枢纽自身 API 401 → 静默续期（下方 request 层，见 board-kit api.ts 非枢纽版注释）
//  - /login 回跳前 → 先换新 access token 再传给目标服务（pages/Login.tsx）
// 接枢纽 SSO 的非枢纽服务（board-kit / pi-web）一律不自续期，
// 401 → 跳转枢纽 /login 换新凭证。
// （dapp / minishop / c2c 走自带 @wetee/web3auth/react 独立登录，与本枢纽无关）
let refreshInFlight: Promise<string | null> | null = null;

async function refreshHubSession(): Promise<string | null> {
  try {
    const raw = localStorage.getItem("web3auth:session");
    if (!raw) return null;
    const rt = (JSON.parse(raw) as { refreshToken?: string }).refreshToken;
    if (!rt) return null;
    const res = await fetch(`${getAuthApiBase()}/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: rt }),
    });
    if (!res.ok) return null;
    const data = await res.json();
    if (!data?.access_token) return null;
    const s = JSON.parse(raw) as Record<string, unknown>;
    s.token = data.access_token;
    s.refreshToken = data.refresh_token ?? s.refreshToken;
    localStorage.setItem("web3auth:session", JSON.stringify(s));
    return data.access_token as string;
  } catch {
    return null;
  }
}

// 全局单例：枢纽多接口并发 401 时只发一次 refresh
function refreshAccessToken(): Promise<string | null> {
  refreshInFlight =
    refreshInFlight ??
    refreshHubSession().finally(() => {
      refreshInFlight = null;
    });
  return refreshInFlight;
}

// 注册到 board-kit api 的续期注入点：枢纽自身的 API 401 走静默续期
//（非枢纽服务不注册，401 一律跳本枢纽 /login 换新凭证）
setRefreshHandler(refreshAccessToken);

// ---------------------------------------------------------------------------
// 统一认证（apps/auth :8008）接入层 — 枢纽（board-ops :5181）版
//
// 全系统唯一的登录框宿主：复用 web3auth 前端 SDK 的登录能力（LoginModal / OTP /
// 社交登录 / 钱包登录），backendUrl 指向 auth 服务；auth 的 /gql 兼容层承接
// SDK 的 GraphQL mutations。/login 页（pages/Login.tsx）调用 login() 弹框。
// 其他服务不引入 web3auth SDK（登录态读本地会话 + 跳转本页，见 board-kit auth.tsx）。
// ---------------------------------------------------------------------------

import { AuthContext, type AuthContextValue } from "./auth/context";

function AuthInner({ children }: { children: ReactNode }) {
  const { isInitialized, isConnected } = useWeb3Auth();
  const { connect } = useWeb3AuthConnect();
  const { disconnect } = useWeb3AuthDisconnect();
  const { userInfo } = useWeb3AuthUser();

  const token = () => {
    try {
      const raw = localStorage.getItem("web3auth:session");
      if (!raw) return "";
      return (JSON.parse(raw) as { token?: string }).token ?? "";
    } catch {
      return "";
    }
  };

  // 角色：web3auth SDK 的 userInfo 不含 roles，登录后调 auth /me 实时拉取
  //（管理页门控用；拉取失败不阻塞登录态，roles 为空）。
  const [roles, setRoles] = useState<string[]>([]);
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

  // 静默续期：枢纽单例（唯一合法轮换者，见上方注释；并发 401 只发一次 refresh）
  const refresh = (): Promise<string | null> => refreshAccessToken();

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
    await disconnect();
  };

  const value = useMemo<AuthContextValue>(
    () => ({
      isInitialized,
      isConnected,
      isConnecting: false,
      connectError: null,
      user: isConnected
        ? {
            id: (userInfo?.verifierId as string) || (userInfo?.email as string) || "",
            email: userInfo?.email,
            name: userInfo?.name,
            roles,
          }
        : null,
      login: connect,
      logout,
      token,
      refresh,
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [isInitialized, isConnected, userInfo, roles],
  );

  return (
    <AuthContext.Provider value={value}>
      <SsoGate>{children}</SsoGate>
    </AuthContext.Provider>
  );
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const config = useMemo<Web3AuthContextConfig>(() => {
    const googleClientId = import.meta.env.VITE_GOOGLE_CLIENT_ID as string | undefined;
    const githubClientId = import.meta.env.VITE_GITHUB_CLIENT_ID as string | undefined;
    const twitterClientId = import.meta.env.VITE_TWITTER_CLIENT_ID as string | undefined;
    const origin = window.location.origin;
    return {
      web3AuthOptions: {
        clientId: "auth",
        // board 是内部管理台，不需要链/钱包；chains 留空（SDK 仅登录路径使用）
        chains: [],
        uiConfig: { mode: "dark", appName: "workflow" },
        authBackendUrl: getAuthApiBase(),
        googleClientId: googleClientId || undefined,
        googleRedirectUri: googleClientId ? `${origin}/oauth/callback.html` : undefined,
        githubClientId: githubClientId || undefined,
        githubRedirectUri: githubClientId ? `${origin}/oauth/callback.html` : undefined,
        twitterClientId: twitterClientId || undefined,
        twitterRedirectUri: twitterClientId ? `${origin}/oauth/callback.html` : undefined,
      },
    };
  }, []);

  return (
    <Web3AuthProvider config={config}>
      <AuthInner>{children}</AuthInner>
    </Web3AuthProvider>
  );
}


