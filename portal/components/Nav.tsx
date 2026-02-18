import Link from "next/link";

const links = [
  ["Deployments", "/deployments"],
  ["Builds", "/builds"],
  ["API Keys", "/api-keys"],
  ["Assistants", "/assistants"],
  ["Threads", "/threads"],
  ["Runs", "/runs"],
  ["Attention", "/attention"]
] as const;

export function Nav() {
  return (
    <nav className="nav">
      {links.map(([label, href]) => (
        <Link key={href} href={href}>
          {label}
        </Link>
      ))}
    </nav>
  );
}
