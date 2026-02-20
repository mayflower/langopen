import type { ReactNode } from "react";

export function AppShell({ nav, children }: { nav: ReactNode; children: ReactNode }) {
  return (
    <div className="app-shell">
      <div className="app-shell__nav">{nav}</div>
      <div className="app-shell__content">{children}</div>
    </div>
  );
}
