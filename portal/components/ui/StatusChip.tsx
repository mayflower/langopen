import type { UiStatus } from "../../lib/ui-types";

export function StatusChip({ status }: { status: UiStatus | string }) {
  const normalized = normalize(status);
  return <span className={`ui-status ui-status--${normalized}`}>{status || "unknown"}</span>;
}

function normalize(input: string): UiStatus {
  const v = (input || "").toLowerCase();
  if (["ok", "success", "healthy", "active"].includes(v)) {
    return "ok";
  }
  if (["running", "in_progress"].includes(v)) {
    return "running";
  }
  if (["queued", "pending"].includes(v)) {
    return "queued";
  }
  if (["warning", "degraded", "interrupted"].includes(v)) {
    return "warning";
  }
  if (["error", "failed", "dead", "revoked"].includes(v)) {
    return "error";
  }
  return "unknown";
}
