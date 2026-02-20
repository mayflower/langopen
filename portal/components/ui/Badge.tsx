import type { ReactNode } from "react";

export function Badge({ children }: { children: ReactNode }) {
  return <span className="ui-badge">{children}</span>;
}
