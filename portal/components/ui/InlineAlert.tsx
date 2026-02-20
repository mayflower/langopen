import type { ReactNode } from "react";

export function InlineAlert({ type, title, children }: { type: "info" | "success" | "warning" | "error"; title?: string; children: ReactNode }) {
  return (
    <div className={`ui-alert ui-alert--${type}`} role={type === "error" ? "alert" : "status"}>
      {title ? <strong>{title}</strong> : null}
      <span>{children}</span>
    </div>
  );
}
