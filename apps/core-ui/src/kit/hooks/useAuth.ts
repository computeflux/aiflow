import { useContext } from "react";
import { AuthContext, type AuthContextValue } from "../auth/context";

// useAuth：读取 board-kit auth context（独立文件：auth.tsx 只导出组件，满足 fast-refresh 规则）
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
