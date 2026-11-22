import { ChevronLeft, ChevronRight } from "lucide-react";

interface PaginationProps {
  page: number;
  pageSize: number;
  total: number;
  onPageChange: (page: number) => void;
}

export function Pagination({ page, pageSize, total, onPageChange }: PaginationProps) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  return (
    <div className="flex items-center justify-between px-4 py-3 border-t border-(--border-subtle) text-xs">
      <span className="text-(--text-muted)">
        共 {total} 条，第 {page}/{totalPages} 页
      </span>
      <div className="flex items-center gap-1">
        <button
          onClick={() => onPageChange(page - 1)}
          disabled={page <= 1}
          className="p-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white disabled:opacity-30 disabled:cursor-not-allowed"
        >
          <ChevronLeft className="h-3 w-3" />
        </button>
        <span className="px-2 text-(--text-secondary)">{page}</span>
        <button
          onClick={() => onPageChange(page + 1)}
          disabled={page >= totalPages}
          className="p-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white disabled:opacity-30 disabled:cursor-not-allowed"
        >
          <ChevronRight className="h-3 w-3" />
        </button>
      </div>
    </div>
  );
}
