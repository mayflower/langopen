export function formatDateTime(input?: string): string {
  if (!input) {
    return "-";
  }
  const d = new Date(input);
  if (Number.isNaN(d.getTime())) {
    return input;
  }
  return d.toLocaleString();
}

export function formatRelativeTime(input?: string): string {
  if (!input) {
    return "-";
  }
  const ts = new Date(input).getTime();
  if (Number.isNaN(ts)) {
    return input;
  }
  const deltaMs = ts - Date.now();
  const abs = Math.abs(deltaMs);
  const mins = Math.round(abs / 60000);
  if (mins < 1) {
    return "just now";
  }
  if (mins < 60) {
    return deltaMs < 0 ? `${mins}m ago` : `in ${mins}m`;
  }
  const hours = Math.round(mins / 60);
  if (hours < 24) {
    return deltaMs < 0 ? `${hours}h ago` : `in ${hours}h`;
  }
  const days = Math.round(hours / 24);
  return deltaMs < 0 ? `${days}d ago` : `in ${days}d`;
}

export function compactId(input?: string, keep = 8): string {
  if (!input) {
    return "-";
  }
  if (input.length <= keep * 2 + 1) {
    return input;
  }
  return `${input.slice(0, keep)}…${input.slice(-keep)}`;
}

export function formatNumber(input: number): string {
  return new Intl.NumberFormat().format(input);
}
