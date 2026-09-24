import type {
  Account,
  AccountDetail,
  BatchRow,
  ClientKey,
  Dashboard,
  Group,
  LogEntry,
  ModelRow,
  ProxyItem,
  SchemaField,
  TaskItem,
  UsageAccount,
  EmulationState,
  EmulationConfig,
} from "@/types/api";

export class ApiError extends Error {
  status: number;
  payload: unknown;
  mustChange: boolean;
  constructor(status: number, message: string, payload?: unknown) {
    super(message);
    this.status = status;
    this.payload = payload;
    this.mustChange = Boolean((payload as { must_change_password?: boolean } | undefined)?.must_change_password);
  }
}

async function parse(res: Response) {
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    const msg =
      data?.error?.message || data?.error || data?.message || res.statusText || "请求失败";
    throw new ApiError(res.status, typeof msg === "string" ? msg : JSON.stringify(msg), data);
  }
  return data;
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "include",
    ...init,
    headers: {
      Accept: "application/json",
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...init?.headers,
    },
  });
  return parse(res) as Promise<T>;
}

export const adminApi = {
  login: (username: string, password: string) =>
    api<{ ok: boolean; username: string; must_change_password?: boolean }>("/api/admin/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }),
  logout: () => api("/api/admin/logout", { method: "POST" }),
  me: () => api<{ ok: boolean; username: string; must_change_password?: boolean }>("/api/admin/me"),
  password: (new_password: string) =>
    api("/api/admin/password", { method: "PUT", body: JSON.stringify({ new_password }) }),

  dashboard: () => api<Dashboard>("/api/admin/dashboard"),
  stats: () => api<Record<string, unknown>>("/api/admin/stats"),

  accounts: () => api<{ accounts: Account[]; total: number }>("/api/admin/accounts"),
  account: (name: string) => api<AccountDetail>(`/api/admin/accounts/${encodeURIComponent(name)}`),
  createAccount: (body: { name?: string; email?: string; api_key?: string; access_token?: string; session_token?: string }) =>
    api<{ name: string; login_url?: string; uuid?: string; logged_in?: boolean }>("/api/admin/accounts", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  patchAccount: (body: Record<string, unknown>) =>
    api<{ ok: boolean; account: Account }>("/api/admin/accounts", {
      method: "PATCH",
      body: JSON.stringify(body),
    }),
  deleteAccount: (name: string) =>
    api(`/api/admin/accounts/${encodeURIComponent(name)}`, { method: "DELETE" }),
  importAccounts: (body: { accounts?: unknown[]; text?: string; overwrite?: boolean }) =>
    api<{ task: { id: string } }>("/api/admin/accounts/import", { method: "POST", body: JSON.stringify(body) }),
  exportAccounts: () => api<{ accounts: unknown[]; total: number }>("/api/admin/accounts/export"),
  accountActions: (action: string, names: string[], extra?: Record<string, unknown>) =>
    api<{ task: TaskItem }>("/api/admin/accounts/actions", {
      method: "POST",
      body: JSON.stringify({ action, names, ...extra }),
    }),
  refreshAccountUsage: (name: string) =>
    api<UsageAccount>(`/api/admin/accounts/${encodeURIComponent(name)}/usage/refresh`, { method: "POST" }),

  keys: () => api<{ keys: ClientKey[]; total: number }>("/api/admin/client-keys"),
  key: (id: string) => api<ClientKey>(`/api/admin/client-keys/${id}`),
  createKey: (body: { name: string; description?: string; group?: string }) =>
    api<{ id: string; key: string; name: string; created_at?: string }>("/api/admin/client-keys", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  updateKey: (id: string, body: Record<string, unknown>) =>
    api<ClientKey>(`/api/admin/client-keys/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteKey: (id: string) => api(`/api/admin/client-keys/${id}`, { method: "DELETE" }),

  groups: () => api<{ groups: Group[]; total: number }>("/api/admin/groups"),
  group: (name: string) => api<Group>(`/api/admin/groups/${encodeURIComponent(name)}`),
  createGroup: (body: { name: string; description?: string }) =>
    api<Group>("/api/admin/groups", { method: "POST", body: JSON.stringify(body) }),
  updateGroup: (name: string, body: Record<string, unknown>) =>
    api<Group>(`/api/admin/groups/${encodeURIComponent(name)}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteGroup: (name: string, force?: boolean) =>
    api(`/api/admin/groups/${encodeURIComponent(name)}${force ? "?force=1" : ""}`, { method: "DELETE" }),

  logs: (q: Record<string, string | number | undefined>) => {
    const p = new URLSearchParams();
    Object.entries(q).forEach(([k, v]) => {
      if (v !== undefined && v !== "") p.set(k, String(v));
    });
    return api<{ entries: LogEntry[]; count: number }>(`/api/admin/logs?${p.toString()}`);
  },
  log: (id: string | number) => api<LogEntry>(`/api/admin/logs/${id}`),
  clearLogs: () => api("/api/admin/logs", { method: "DELETE" }),

  usage: (refresh?: boolean) =>
    api<{ total_success: number; accounts: UsageAccount[] }>(`/api/admin/usage${refresh ? "?refresh=1" : ""}`),
  usageRefreshTask: () => api<{ task: TaskItem }>("/api/admin/usage/refresh", { method: "POST" }),

  config: () => api<Record<string, unknown>>("/api/admin/config"),
  configSchema: () => api<{ fields: SchemaField[] }>("/api/admin/config/schema"),
  updateConfig: (patch: Record<string, unknown>) =>
    api<{ config: Record<string, unknown>; restart_required: boolean }>("/api/admin/config", {
      method: "PUT",
      body: JSON.stringify(patch),
    }),

  models: () => api<{ models: ModelRow[]; total: number }>("/api/admin/models"),
  modelsRefreshTask: () => api<{ task: TaskItem }>("/api/admin/models/refresh", { method: "POST" }),

  batches: () => api<{ batches: BatchRow[]; total: number }>("/api/admin/batches"),
  batch: (id: string) => api<BatchRow>(`/api/admin/batches/${id}`),

  proxies: () => api<{ proxies: ProxyItem[]; total: number }>("/api/admin/proxies"),
  proxy: (id: string) => api<ProxyItem>(`/api/admin/proxies/${id}`),
  createProxy: (body: Record<string, unknown>) =>
    api<ProxyItem>("/api/admin/proxies", { method: "POST", body: JSON.stringify(body) }),
  patchProxy: (id: string, body: Record<string, unknown>) =>
    api<ProxyItem>(`/api/admin/proxies/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteProxy: (id: string) => api(`/api/admin/proxies/${id}`, { method: "DELETE" }),
  probeProxy: (id: string) => api<{ task: TaskItem }>(`/api/admin/proxies/${id}/probe`, { method: "POST" }),

  tasks: () => api<{ tasks: TaskItem[]; total: number; concurrency: number }>("/api/admin/tasks"),
  task: (id: string) => api<TaskItem>(`/api/admin/tasks/${id}`),
  cancelTask: (id: string) => api(`/api/admin/tasks/${id}/cancel`, { method: "POST" }),

  emulation: () => api<EmulationState>("/api/admin/emulation"),
  updateEmulation: (config: EmulationConfig) =>
    api<EmulationState>("/api/admin/emulation", { method: "PUT", body: JSON.stringify(config) }),
  testWebSearch: (query: string) =>
    api<{ ok: boolean; provider: string; results?: { title: string; url: string; snippet: string }[]; answer?: string; latency_ms: number }>(
      "/api/admin/emulation/websearch/test",
      { method: "POST", body: JSON.stringify({ query }) },
    ),
};
