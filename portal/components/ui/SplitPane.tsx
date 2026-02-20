import type { ReactNode } from "react";

export function SplitPane({ left, right }: { left: ReactNode; right: ReactNode }) {
  return (
    <section className="ui-split-pane">
      <div>{left}</div>
      <div>{right}</div>
    </section>
  );
}
