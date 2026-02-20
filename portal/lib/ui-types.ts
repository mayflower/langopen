export type UiStatus = "ok" | "running" | "queued" | "warning" | "error" | "unknown";

export type DataState<T> = {
  loading: boolean;
  error: string;
  items: T[];
};

export type ActionPhase = "idle" | "pending" | "success" | "error";

export type ActionState = {
  phase: ActionPhase;
  message: string;
};

export function initialDataState<T>(): DataState<T> {
  return { loading: false, error: "", items: [] };
}

export function initialActionState(): ActionState {
  return { phase: "idle", message: "" };
}
