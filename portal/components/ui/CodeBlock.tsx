import type { ReactNode } from "react";

export function CodeBlock({ title, children }: { title?: string; children: ReactNode }) {
  return (
    <section className="ui-codeblock">
      {title ? <header>{title}</header> : null}
      <pre>{children}</pre>
    </section>
  );
}
