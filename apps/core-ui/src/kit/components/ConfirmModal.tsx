import { useState } from "react";

interface ConfirmModalProps {
  open: boolean;
  title: string;
  message: string;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
}

export function ConfirmModal({ open, title, message, onConfirm, onCancel }: ConfirmModalProps) {
  const [pending, setPending] = useState(false);
  // 打开时复位 pending：关闭态不渲染，重开时按 key 重挂载会自然复位；这里用 key 保证同实例重开也复位
  const [lastOpen, setLastOpen] = useState(open);
  if (lastOpen !== open) {
    setLastOpen(open);
    if (!open) setPending(false); // render 期 setState（React 官方 reset-on-prop-change 模式），避免 effect 级联渲染
  }

  if (!open) return null;

  const handleConfirm = async () => {
    try {
      setPending(true);
      await onConfirm();
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onCancel} />
      <div className="relative bg-(--bg-app) border border-(--border-default) p-6 w-96 shadow-2xl">
        <h3 className="text-sm font-bold text-(--text-primary) mb-2">{title}</h3>
        <p className="text-xs text-(--text-secondary) mb-6">{message}</p>
        <div className="flex justify-end gap-2">
          <button
            onClick={onCancel}
            disabled={pending}
            className="border border-(--border-default) px-4 py-1.5 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) disabled:opacity-50"
          >
            取消
          </button>
          <button
            onClick={handleConfirm}
            disabled={pending}
            className="bg-(--danger) px-4 py-1.5 text-xs font-bold text-white hover:bg-(--danger)/80 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {pending ? "处理中..." : "确认删除"}
          </button>
        </div>
      </div>
    </div>
  );
}
