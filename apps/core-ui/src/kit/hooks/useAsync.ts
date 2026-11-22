import { useCallback, useEffect, useRef, useState } from "react";

// useAsync：页面级「取数 → loading → error」三态的标准封装。
// 语义与页面惯用手写样板完全同构（fetcher 变化即重新取数 + 首帧取数 + 最新请求胜出）：
// - deps 数组 = 刷新触发条件（等价于 `const load = useCallback(..., deps)` + `useEffect(() => { load(); }, [load])`）；
//   fetcher 经 ref 转发，无需 useCallback 包稳定引用；deps 变化自动重新取数。
// - 内置请求序号守卫：过期响应一律丢弃（不再 setState），与页面 loadSeqRef 行为一致。
// - immediate=false：不自动取数，由调用方自行 reload（如条件挂载的页面）。
export interface AsyncState<T> {
    data: T | null;
    loading: boolean;
    error: string | null;
    /** 手动刷新（保持当前 data，不闪空态） */
    reload: () => Promise<void>;
    /** 静默刷新：不置 loading、失败不报 error（轮询用，避免打断交互） */
    reloadSilently: () => Promise<void>;
    /** 直接写 data（乐观更新用） */
    setData: (updater: T | ((prev: T | null) => T | null)) => void;
    setError: (msg: string | null) => void;
}

export function useAsync<T>(
    fetcher: () => Promise<T>,
    deps: readonly unknown[],
    opts?: { immediate?: boolean },
): AsyncState<T> {
    const immediate = opts?.immediate !== false;
    const [data, setDataState] = useState<T | null>(null);
    const [loading, setLoading] = useState(immediate);
    const [error, setError] = useState<string | null>(null);

    const fetcherRef = useRef(fetcher);
    useEffect(() => {
        fetcherRef.current = fetcher;
    }, [fetcher]);

    const mounted = useRef(true);
    useEffect(() => {
        mounted.current = true;
        return () => {
            mounted.current = false;
        };
    }, []);

    const seqRef = useRef(0);

    const run = useCallback(async (silent: boolean) => {
        const seq = ++seqRef.current;
        if (!silent) setLoading(true);
        try {
            const result = await fetcherRef.current();
            if (!mounted.current || seq !== seqRef.current) return;
            setDataState(result);
            setError(null);
        } catch (err) {
            if (!mounted.current || seq !== seqRef.current || silent) return;
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            if (!silent && mounted.current && seq === seqRef.current) setLoading(false);
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, deps);

    // deps 变化即重新取数（首帧同理；immediate=false 时跳过）
    useEffect(() => {
        if (immediate) void run(false);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [run, immediate]);

    const reload = useCallback(() => run(false), [run]);
    const reloadSilently = useCallback(() => run(true), [run]);

    const setData = useCallback((updater: T | ((prev: T | null) => T | null)) => {
        setDataState((prev) => (typeof updater === "function" ? (updater as (p: T | null) => T | null)(prev) : updater));
    }, []);

    return { data, loading, error, reload, reloadSilently, setData, setError };
}

// useInterval：声明式 setInterval。delay 为 null 时暂停。
// 回调经 ref 转发：闭包永远是最新的，不需要把回调塞进依赖数组。
export function useInterval(callback: () => void, delay: number | null): void {
    const cbRef = useRef(callback);
    useEffect(() => {
        cbRef.current = callback;
    }, [callback]);

    useEffect(() => {
        if (delay === null) return;
        const id = setInterval(() => cbRef.current(), delay);
        return () => clearInterval(id);
    }, [delay]);
}
