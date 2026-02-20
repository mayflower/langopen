"use client";

import { useCallback, useEffect, useState } from "react";
import { usePathname, useRouter } from "next/navigation";

export type TableQueryState = {
  q: string;
  sort: string;
  order: "asc" | "desc";
  page: number;
  pageSize: number;
};

const DEFAULT_STATE: TableQueryState = {
  q: "",
  sort: "",
  order: "asc",
  page: 1,
  pageSize: 10
};

export function useTableQueryState(defaults?: Partial<TableQueryState>) {
  const mergedDefaults = { ...DEFAULT_STATE, ...(defaults || {}) };
  const router = useRouter();
  const pathname = usePathname();
  const [state, setLocalState] = useState<TableQueryState>(mergedDefaults);

  useEffect(() => {
    if (typeof window === "undefined") {
      return;
    }
    const params = new URLSearchParams(window.location.search);
    const page = parseInt(params.get("page") || `${mergedDefaults.page}`, 10);
    const pageSize = parseInt(params.get("pageSize") || `${mergedDefaults.pageSize}`, 10);
    const order = params.get("order") === "desc" ? "desc" : "asc";
    setLocalState({
      q: params.get("q") || mergedDefaults.q,
      sort: params.get("sort") || mergedDefaults.sort,
      order,
      page: Number.isFinite(page) && page > 0 ? page : mergedDefaults.page,
      pageSize: Number.isFinite(pageSize) && pageSize > 0 ? pageSize : mergedDefaults.pageSize
    });
  }, [mergedDefaults.order, mergedDefaults.page, mergedDefaults.pageSize, mergedDefaults.q, mergedDefaults.sort]);

  const setState = useCallback(
    (patch: Partial<TableQueryState>) => {
      const next = { ...state, ...patch };
      const nextParams = new URLSearchParams();
      writeParam(nextParams, "q", next.q, mergedDefaults.q);
      writeParam(nextParams, "sort", next.sort, mergedDefaults.sort);
      writeParam(nextParams, "order", next.order, mergedDefaults.order);
      writeParam(nextParams, "page", `${next.page}`, `${mergedDefaults.page}`);
      writeParam(nextParams, "pageSize", `${next.pageSize}`, `${mergedDefaults.pageSize}`);
      setLocalState(next);
      router.replace(`${pathname}?${nextParams.toString()}`);
    },
    [mergedDefaults.order, mergedDefaults.page, mergedDefaults.pageSize, mergedDefaults.q, mergedDefaults.sort, pathname, router, state]
  );

  return { state, setState };
}

function writeParam(params: URLSearchParams, key: string, value: string, fallback: string) {
  if (!value || value === fallback) {
    params.delete(key);
    return;
  }
  params.set(key, value);
}
