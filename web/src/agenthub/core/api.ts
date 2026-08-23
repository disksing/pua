export const AGENTHUB_BASE_PATH = "/agenthub";

export function agentHubPath(path: string): string {
  const value = String(path || "");
  if (value === AGENTHUB_BASE_PATH || value.startsWith(`${AGENTHUB_BASE_PATH}/`)) return value;
  return `${AGENTHUB_BASE_PATH}${value.startsWith("/") ? value : `/${value}`}`;
}

export async function api<T = any>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(agentHubPath(path), {
    ...options,
    headers: options.body ? { "Content-Type": "application/json", ...options.headers } : options.headers,
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error?.message || response.statusText);
  return body as T;
}
