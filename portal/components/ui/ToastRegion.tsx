"use client";

import { useCallback, useState } from "react";

export type Toast = {
  id: number;
  tone: "success" | "warning" | "error" | "info";
  message: string;
};

export function useToastRegion() {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const push = useCallback((tone: Toast["tone"], message: string) => {
    const id = Date.now() + Math.floor(Math.random() * 1000);
    setToasts((prev) => [...prev, { id, tone, message }]);
    setTimeout(() => {
      setToasts((prev) => prev.filter((t) => t.id !== id));
    }, 3600);
  }, []);

  const remove = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  return { toasts, push, remove };
}

export function ToastRegion({ toasts, remove }: { toasts: Toast[]; remove: (id: number) => void }) {
  if (toasts.length === 0) {
    return null;
  }
  return (
    <div className="ui-toast-region" aria-live="polite">
      {toasts.map((toast) => (
        <div key={toast.id} className={`ui-toast ui-toast--${toast.tone}`}>
          <span>{toast.message}</span>
          <button type="button" onClick={() => remove(toast.id)} aria-label="Dismiss toast">
            ×
          </button>
        </div>
      ))}
    </div>
  );
}
