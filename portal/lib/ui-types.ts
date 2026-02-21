export type UiStatus = "ok" | "running" | "queued" | "warning" | "error" | "unknown";

export type DeploymentRuntimeVariable = {
  id?: string;
  key: string;
  is_secret: boolean;
  value_masked: string;
  updated_at: string;
};

export type DeploymentVariableUpsertRequest = {
  project_id: string;
  key: string;
  value: string;
  is_secret: boolean;
};

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
