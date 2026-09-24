import { RefreshCw } from "lucide-react";
import { ProgressBar } from "@/components/shared/progress-bar";
import { formatReset, formatUSDFromCents } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { Account, VendorUsage } from "@/types/api";

export type UsageBreakdown = {
  usage_percent?: number;
  auto_percent_used?: number;
  api_percent_used?: number;
  plan_used_cents?: number;
  plan_limit_cents?: number;
  on_demand_used_cents?: number;
  on_demand_limit_cents?: number;
  billing_cycle_end?: number;
  unlimited?: boolean;
  usage_error?: string;
};

export function usageFromAccount(a: Account): UsageBreakdown {
  return {
    usage_percent: a.usage_percent,
    auto_percent_used: a.auto_percent_used,
    api_percent_used: a.api_percent_used,
    plan_used_cents: a.plan_used_cents,
    plan_limit_cents: a.plan_limit_cents,
    on_demand_used_cents: a.on_demand_used_cents,
    on_demand_limit_cents: a.on_demand_limit_cents,
    billing_cycle_end: a.billing_cycle_end,
    unlimited: a.unlimited,
    usage_error: a.usage_error,
  };
}

export function usageFromVendor(c?: VendorUsage, err?: string): UsageBreakdown {
  return {
    usage_percent: c?.total_percent_used,
    auto_percent_used: c?.auto_percent_used,
    api_percent_used: c?.api_percent_used,
    plan_used_cents: c?.plan_used_cents,
    plan_limit_cents: c?.plan_limit_cents,
    on_demand_used_cents: c?.on_demand_used_cents,
    on_demand_limit_cents: c?.on_demand_limit_cents,
    billing_cycle_end: c?.billing_cycle_end,
    unlimited: c?.unlimited,
    usage_error: err,
  };
}

export function hasUsageMeters(u: UsageBreakdown) {
  return (
    u.usage_percent != null ||
    u.auto_percent_used != null ||
    u.api_percent_used != null ||
    u.plan_used_cents != null ||
    u.on_demand_used_cents != null
  );
}

function clampPct(n?: number | null) {
  if (n == null || Number.isNaN(n)) return undefined;
  return Math.max(0, Math.min(100, n));
}

function Meter({
  label,
  pct,
  extra,
  muted,
}: {
  label: string;
  pct?: number | null;
  extra?: string;
  muted?: boolean;
}) {
  const bar = clampPct(pct);
  return (
    <div>
      <div className="flex items-baseline justify-between gap-2 text-[12px]">
        <span className="text-ash">{label}</span>
        <span className={muted ? "font-mono text-ash" : "font-mono text-verdant"}>
          {bar == null ? extra || "—" : `${bar.toFixed(0)}%`}
        </span>
      </div>
      {extra && bar != null ? <div className="mt-0.5 font-mono text-[11px] text-ash">{extra}</div> : null}
      <ProgressBar className="mt-1" current={bar ?? 0} total={100} tone="ok" />
    </div>
  );
}

export function UsageMeters({
  usage,
  onRefresh,
  refreshing,
}: {
  usage: UsageBreakdown;
  onRefresh?: () => void | Promise<void>;
  refreshing?: boolean;
}) {
  const total = clampPct(usage.usage_percent);
  const cost =
    usage.plan_used_cents != null && usage.plan_limit_cents != null
      ? `${formatUSDFromCents(usage.plan_used_cents)} / ${formatUSDFromCents(usage.plan_limit_cents)}`
      : usage.plan_used_cents != null
        ? formatUSDFromCents(usage.plan_used_cents)
        : undefined;
  const reset = formatReset(usage.billing_cycle_end);
  const odUsed = usage.on_demand_used_cents;
  const odLimit = usage.on_demand_limit_cents;
  const odPct = odUsed != null && odLimit != null && odLimit > 0 ? (odUsed / odLimit) * 100 : null;
  const odExtra =
    odUsed != null && odLimit != null
      ? `${formatUSDFromCents(odUsed)} / ${formatUSDFromCents(odLimit)}`
      : odUsed != null
        ? formatUSDFromCents(odUsed)
        : undefined;

  if (!hasUsageMeters(usage)) {
    return (
      <div className="space-y-2">
        {usage.usage_error && !refreshing ? <p className="text-[12px] text-crimson">{usage.usage_error}</p> : null}
        <button
          type="button"
          disabled={refreshing || !onRefresh}
          onClick={onRefresh}
          className="flex min-h-12 w-full items-center justify-center gap-2 text-[13px] text-ash hover:text-ink disabled:opacity-70"
        >
          <RefreshCw className={cn("size-4", refreshing && "animate-spin")} />
          {refreshing ? "正在刷新额度…" : "点击此处刷新额度"}
        </button>
      </div>
    );
  }

  return (
    <div className="relative min-h-[4.5rem] space-y-2.5">
      {refreshing ? (
        <div className="absolute inset-0 z-10 grid place-items-center rounded-md bg-bone/85">
          <span className="inline-flex items-center gap-2 text-[13px] text-ash">
            <RefreshCw className="size-4 animate-spin" />
            正在刷新额度…
          </span>
        </div>
      ) : null}
      <Meter
        label="总用量"
        pct={usage.unlimited ? null : total}
        extra={
          usage.unlimited
            ? "不限"
            : [cost, reset ? `重置 ${reset}` : ""].filter(Boolean).join(" · ")
        }
        muted={usage.unlimited}
      />
      <Meter label="Auto + Composer" pct={usage.auto_percent_used} />
      <Meter label="API" pct={usage.api_percent_used} />
      <Meter label="按需使用" pct={odPct} extra={odExtra} muted={odPct == null} />
      {usage.usage_error && !refreshing ? <p className="text-[12px] text-crimson">{usage.usage_error}</p> : null}
    </div>
  );
}
