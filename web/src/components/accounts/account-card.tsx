import { Boxes, RefreshCw, Settings2, Trash2 } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Switch } from "@/components/ui/switch";
import { UsageMeters, usageFromAccount } from "@/components/shared/usage-meters";
import { formatDisableReason, formatTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { Account } from "@/types/api";

function planTone(label?: string) {
  const n = (label || "").toLowerCase();
  if (n.includes("ultra")) return "ember";
  if (n.includes("pro") || n.includes("team") || n.includes("enterprise")) return "ok";
  if (n.includes("free")) return "muted";
  return "warn";
}

export function AccountCard({
  account,
  checked,
  onCheck,
  onToggle,
  onRefresh,
  onDelete,
}: {
  account: Account;
  checked: boolean;
  onCheck: (v: boolean) => void;
  onToggle: (enabled: boolean) => void;
  onRefresh: () => void | Promise<void>;
  onDelete: () => void;
}) {
  const a = account;
  const cooling = Boolean(a.cooldown_until && a.cooldown_until * 1000 > Date.now());
  const title = a.email || a.name;
  const plan = a.plan_label || a.plan_badge;
  const [refreshing, setRefreshing] = useState(false);

  const refresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await onRefresh();
    } finally {
      setRefreshing(false);
    }
  };

  return (
    <Card interactive className="flex h-full flex-col p-3">
      <div className="flex items-start gap-2">
        <Checkbox checked={checked} onCheckedChange={(v) => onCheck(v === true)} />
        <div className="grid size-7 shrink-0 place-items-center rounded-md bg-linen font-mono text-[11px] text-ink">
          {(title[0] || "A").toUpperCase()}
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <Badge tone={planTone(plan)} className="rounded-sm border border-border px-1.5 py-0.5">
              {plan || (a.logged_in ? "未同步套餐" : "未登录")}
            </Badge>
            {a.groups?.slice(0, 1).map((g) => (
              <Badge key={g} className="rounded-sm border border-border px-1.5 py-0.5">
                {g}
              </Badge>
            ))}
          </div>
          <Link to={`/accounts/${encodeURIComponent(a.name)}`} className="mt-1 block truncate text-[14px] text-ink hover:text-ember">
            {title}
          </Link>
          {a.workos_id ? <div className="truncate font-mono text-[11px] text-ash">Auth ID {a.workos_id}</div> : a.email && a.email !== a.name ? <div className="truncate font-mono text-[11px] text-ash">{a.name}</div> : null}
        </div>
        <div className="shrink-0 text-right">
          <Badge tone={a.enabled ? (cooling ? "warn" : "ok") : "muted"} className="rounded-sm border border-border px-1.5 py-0.5">
            {a.enabled ? (cooling ? "冷却" : "启用") : "停用"}
          </Badge>
          {!a.enabled && a.disable_reason ? (
            <div className="mt-1 max-w-[9rem] truncate text-[11px] text-ash">{formatDisableReason(a.disable_reason)}</div>
          ) : null}
        </div>
      </div>

      <div className="mt-3 flex items-center justify-between text-[12px]">
        <span className="text-ash">健康状态</span>
        <span className="font-mono">
          <span className="text-verdant">成功 {a.success_count ?? 0}</span>
          <span className="mx-1.5 text-border">/</span>
          <span className="text-crimson">失败 {a.fail_count ?? 0}</span>
        </span>
      </div>
      <div className="mt-1.5 flex gap-0.5">
        {Array.from({ length: 12 }).map((_, i) => {
          const total = (a.success_count ?? 0) + (a.fail_count ?? 0);
          const failShare = total === 0 ? 0 : (a.fail_count ?? 0) / total;
          const bad = i >= Math.round(12 * (1 - failShare));
          return <span key={i} className={cn("h-1.5 flex-1 rounded-[2px]", total === 0 ? "bg-linen" : bad ? "bg-crimson/70" : "bg-verdant/80")} />;
        })}
      </div>

      <div className="mt-2 flex flex-wrap items-center gap-1.5 font-mono text-[11px] text-ash">
        <span className="rounded-sm border border-border px-1.5 py-0.5">优先级 {a.priority ?? 0}</span>
        <span>权重 {a.weight ?? 1}</span>
        {a.proxy_id ? <span>出口 {a.proxy_id}</span> : null}
        <span>{formatTime(a.usage_updated_at || a.expires_at)}</span>
      </div>

      <div className="mt-1.5 flex flex-wrap items-center gap-1.5 font-mono text-[11px]">
        <span className="rounded-sm border border-border px-1.5 py-0.5 text-ash">
          并发 <span className={a.inflight && a.inflight > 0 ? "text-ink" : ""}>{a.inflight ?? 0}</span>/{a.concurrency_limit && a.concurrency_limit > 0 ? a.concurrency_limit : "∞"}
        </span>
        {a.concurrency_degraded ? (
          <Badge tone="warn" className="rounded-sm border border-border px-1.5 py-0.5">
            429 降级
          </Badge>
        ) : null}
      </div>

      <div className="mt-2 flex-1 rounded-md border border-border/70 bg-parchment px-2.5 py-2">
        <UsageMeters usage={usageFromAccount(a)} onRefresh={refresh} refreshing={refreshing} />
      </div>

      <div className="mt-2 flex items-center gap-1">
        <Button variant="outline" size="sm" className="h-8 px-2" asChild>
          <Link to="/models">
            <Boxes /> 模型
          </Link>
        </Button>
        <Button variant="outline" size="icon" className="size-8" aria-label="刷新用量" disabled={refreshing} onClick={refresh}>
          <RefreshCw className={cn(refreshing && "animate-spin")} />
        </Button>
        <Button variant="outline" size="icon" className="size-8" aria-label="设置" asChild>
          <Link to={`/accounts/${encodeURIComponent(a.name)}`}>
            <Settings2 />
          </Link>
        </Button>
        <Button variant="outline" size="icon" className="size-8" aria-label="删除" onClick={onDelete}>
          <Trash2 className="text-crimson" />
        </Button>
        <div className="ml-auto flex items-center gap-1.5">
          <span className="text-[12px] text-ash">启用</span>
          <Switch checked={a.enabled} onCheckedChange={onToggle} />
        </div>
      </div>
    </Card>
  );
}
