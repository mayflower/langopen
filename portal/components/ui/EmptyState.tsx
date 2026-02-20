import type { ReactNode } from "react";

export function EmptyState({ title, description, action }: { title: string; description: string; action?: ReactNode }) {
  return (
    <section className="ui-empty">
      <h3>{title}</h3>
      <p>{description}</p>
      {action ? <div className="ui-empty__action">{action}</div> : null}
    </section>
  );
}
