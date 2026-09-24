export type Account = {
  name: string;
  id?: string;
  logged_in: boolean;
  expires_at?: number;
  fail_count?: number;
  disabled?: boolean;
  enabled: boolean;
  has_api_key?: boolean;
  priority?: number;
  weight?: number;
  groups?: string[];
  fail_class?: string;
  cooldown_until?: number;
  cooldown_reason?: string;
  trigger_count?: number;
  success_count?: number;
  proxy_url?: string;
  proxy_id?: string;
  rpm?: number;
  daily_max?: number;
  disable_reason?: string;
  client_type?: string;
  email?: string;
  plan_badge?: string;
  plan_label?: string;
  membership_type?: string;
  workos_id?: string;
  usage_percent?: number;
  auto_percent_used?: number;
  api_percent_used?: number;
  plan_used_cents?: number;
  plan_limit_cents?: number;
  on_demand_used_cents?: number;
  on_demand_limit_cents?: number;
  usage_error?: string;
  usage_updated_at?: number;
  billing_cycle_end?: number;
  unlimited?: boolean;
  inflight?: number;
  concurrency_limit?: number;
  concurrency_degraded?: boolean;
};

export type LogEntry = {
  id: number;
  time: string;
  ip: string;
  method: string;
  path: string;
  status: number;
  account?: string;
  model?: string;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  latency_ms: number;
  error?: string;
  client_key_id?: number;
  fail_class?: string;
  retry_count?: number;
  request_id?: string;
  stream?: boolean;
  headers?: Record<string, string>;
  request_body?: string;
};

export type Dashboard = {
  uptime_seconds: number;
  accounts: {
    total: number;
    enabled: number;
    disabled: number;
    cooling: number;
    ready: number;
  };
  cooldowns: {
    name: string;
    reason: string;
    fail_class?: string;
    until: number;
  }[];
  refresh_failures?: Record<string, number>;
  stats: DashboardStats;
};

export type DashboardStats = {
  total_requests: number;
  error_requests: number;
  error_rate: number;
  total_prompt: number;
  total_completion: number;
  total_tokens: number;
  avg_latency_ms: number;
  by_account: Record<string, number>;
  by_model: Record<string, number>;
  by_path: Record<string, number>;
  by_fail_class?: Record<string, number>;
  by_client_key?: Record<string, number>;
  tokens_by_account?: Record<string, number>;
  tokens_by_model?: Record<string, number>;
};

export type ClientKey = {
  id: string;
  masked_key: string;
  name: string;
  description?: string;
  disabled: boolean;
  group?: string;
  total_calls: number;
  total_tokens: number;
  created_at?: string;
  last_used_at?: string;
  is_system?: boolean;
};

export type Group = {
  name: string;
  description?: string;
  created_at?: string;
  rpm?: number;
  daily_max?: number;
  credential_count: number;
  client_key_count: number;
};

export type VendorUsage = {
  email?: string;
  workos_id?: string;
  membership_type?: string;
  plan_badge?: string;
  plan_label?: string;
  subscription_status?: string;
  total_percent_used?: number;
  auto_percent_used?: number;
  api_percent_used?: number;
  plan_used_cents?: number;
  plan_limit_cents?: number;
  on_demand_used_cents?: number;
  on_demand_limit_cents?: number;
  billing_cycle_end?: number;
  unlimited?: boolean;
  fetched_at?: number;
};

export type UsageAccount = {
  name?: string;
  success_count: number;
  vendor?: VendorUsage;
  error?: string;
  usage_error?: string;
  usage_updated_at?: number;
  updated_at?: number;
};

export type ProxyItem = {
  id: string;
  name: string;
  kind: "resin" | "http" | "direct" | string;
  enabled: boolean;
  is_default?: boolean;
  notes?: string;
  http_proxy?: string;
  resin_url?: string;
  resin_platform?: string;
  last_probe_ip?: string;
  last_probe_at?: number;
  last_error?: string;
  created_at: string;
  updated_at: string;
};

export type TaskItem = {
  id: string;
  type: string;
  title: string;
  status: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  current: number;
  total: number;
  message?: string;
  error?: string;
  result?: unknown;
  meta?: Record<string, unknown>;
  logs: { time: string; level: string; message: string }[];
  created_by?: string;
  cancelable: boolean;
};

export type SchemaField = {
  name: string;
  type: string;
  restart_required?: boolean;
  secret?: boolean;
  default?: unknown;
  description: string;
};

export type ModelRow = {
  id: string;
  display_name: string;
  server_model_name: string;
  aliases?: string[];
  supports_thinking: boolean;
  supports_images: boolean;
  supports_agent: boolean;
  context_token_limit?: number;
  thinking_level?: string;
  max_mode?: boolean;
};

export type BatchRow = {
  id: string;
  type: string;
  processing_status: string;
  request_counts: {
    processing: number;
    succeeded: number;
    errored: number;
    canceled: number;
    expired: number;
  };
  created_at: string;
  expires_at: string;
  ended_at?: string;
};

export type AccountDetail = {
  account: Account;
  usage: UsageAccount;
  logs: LogEntry[];
  egress?: { Kind?: string; kind?: string; ResinURL?: string; HTTPProxyURL?: string; Account?: string };
};

export type TavilyKey = {
  id: string;
  name?: string;
  key?: string;
  enabled: boolean;
  last_error?: string;
  last_used_at?: string;
};

export type EmulationConfig = {
  cache: {
    enabled: boolean;
    mode: "uniform" | "independent";
    ratio: number;
    ratio_min?: number;
    ratio_max?: number;
    creation_ratio: number;
    read_ratio: number;
    read_ratio_min?: number;
    read_ratio_max?: number;
    force_hit?: boolean;
    min_tokens: number;
    opus_min_tokens: number;
    fallback_breakpoints: boolean;
    ttl_5m_seconds: number;
    ttl_1h_seconds: number;
  };
  web_search: {
    enabled: boolean;
    provider: "vendor" | "tavily" | "auto";
    max_results: number;
    tavily_keys?: TavilyKey[];
  };
  signature: { enabled: boolean };
};

export type CacheSplit = {
  input_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
};

export type CacheLog = {
  time: string;
  model: string;
  total?: number;
  input: number;
  read: number;
  creation: number;
  hit: boolean;
};

export type SearchLog = {
  time: string;
  query: string;
  provider: string;
  results: number;
  error?: string;
  latency_ms: number;
};

export type EmulationState = {
  config: EmulationConfig;
  cache_log?: CacheLog[];
  search_log?: SearchLog[];
};
