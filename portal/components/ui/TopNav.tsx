"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useMemo, useState } from "react";
import { NAV_ITEMS } from "../../lib/nav";

type AttentionSummary = {
  stuck_runs: number;
  pending_runs: number;
  error_runs: number;
};

export function TopNav() {
  const pathname = usePathname();
  const [attentionCount, setAttentionCount] = useState(0);

  useEffect(() => {
    let active = true;
    void (async () => {
      try {
        const resp = await fetch("/api/platform/data/api/v1/system/attention", { cache: "no-store" });
        if (!resp.ok) {
          return;
        }
        const json = (await resp.json()) as Partial<AttentionSummary>;
        const next = (json.stuck_runs || 0) + (json.pending_runs || 0) + (json.error_runs || 0);
        if (active) {
          setAttentionCount(next);
        }
      } catch {
      }
    })();
    return () => {
      active = false;
    };
  }, [pathname]);

  const normalizedPath = useMemo(() => {
    if (!pathname || pathname === "/") {
      return "/deployments";
    }
    return pathname;
  }, [pathname]);

  return (
    <nav className="top-nav" aria-label="Primary">
      {NAV_ITEMS.map((item) => {
        const active = normalizedPath === item.href;
        return (
          <Link key={item.href} href={item.href} className={active ? "is-active" : ""}>
            <span>{item.label}</span>
            {item.attentionKey === "attention" && attentionCount > 0 ? (
              <span className="top-nav__badge" aria-label={`${attentionCount} items need attention`}>
                {attentionCount}
              </span>
            ) : null}
          </Link>
        );
      })}
    </nav>
  );
}
