import { Component, type ReactNode } from "react";

interface ErrorBoundaryProps {
    children: ReactNode;
}

interface ErrorBoundaryState {
    error: Error | null;
}

export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
    state: ErrorBoundaryState = { error: null };

    static getDerivedStateFromError(error: Error): ErrorBoundaryState {
        return { error };
    }

    render() {
        if (this.state.error) {
            return (
                <div className="flex h-screen flex-col items-center justify-center gap-4 bg-(--bg-app) text-[#FAFAFA] px-4">
                    <h1 className="text-2xl font-bold tracking-tighter uppercase">页面出现异常</h1>
                    <p className="text-sm text-(--text-muted)">渲染出错，请刷新页面重试</p>
                    <pre className="max-w-2xl w-full overflow-auto border border-(--border-subtle) bg-(--bg-elevated) p-4 text-xs text-red-400 whitespace-pre-wrap break-words">
                        {this.state.error.message}
                    </pre>
                    <button
                        onClick={() => window.location.reload()}
                        className="border border-(--border-subtle) px-6 py-2 text-sm font-bold text-(--text-secondary) hover:bg-(--bg-elevated) hover:text-white transition"
                    >
                        刷新
                    </button>
                </div>
            );
        }
        return this.props.children;
    }
}
