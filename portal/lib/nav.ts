export type NavItem = {
  label: string;
  href: string;
  attentionKey?: "deployments" | "builds" | "apiKeys" | "assistants" | "threads" | "runs" | "attention";
};

export const NAV_ITEMS: NavItem[] = [
  { label: "Deployments", href: "/deployments", attentionKey: "deployments" },
  { label: "Builds", href: "/builds", attentionKey: "builds" },
  { label: "API Keys", href: "/api-keys", attentionKey: "apiKeys" },
  { label: "Assistants", href: "/assistants", attentionKey: "assistants" },
  { label: "Threads", href: "/threads", attentionKey: "threads" },
  { label: "Runs", href: "/runs", attentionKey: "runs" },
  { label: "Attention", href: "/attention", attentionKey: "attention" }
];
