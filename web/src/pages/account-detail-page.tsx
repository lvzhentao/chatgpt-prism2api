import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { GroupChecks } from "@/components/shared/group-select";
import { JsonView } from "@/components/shared/json-view";
import { PageHeader } from "@/components/shared/page-header";
import { UsageMeters, hasUsageMeters, usageFromAccount, usageFromVendor } from "@/components/shared/usage-meters";
import { adminApi } from "@/lib/api";
import { formatDisableReason, formatTime } from "@/lib/format";
import { cn } from "@/lib/utils";

export function AccountDetailPage() {
  const { name = "" } = useParams();
  const decoded = decodeURIComponent(name);
  const nav = useNavigate();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["account", decoded], queryFn: () => adminApi.account(decoded) });
  const proxies = useQuery({ queryKey: ["proxies"], queryFn: adminApi.proxies });
  const groups = useQuery({ queryKey: ["groups"], queryFn: adminApi.groups });
  const [prio, setPrio] = useState<string>();
  const [weight, setWeight] = useState<string>();
  const [rpm, setRpm] = useState<string>();
  const [daily, setDaily] = useState<string>();
  const [proxyId, setProxyId] = useState<string>();
  const [groupPicked, setGroupPicked] = useState<string[]>();
  const [refreshing, setRefreshing] = useState(false);

  const save = useMutation({
    mutationFn: () =>
      adminApi.patchAccount({
        name: decoded,
        priority: prio != null ? Number(prio) : undefined,
        weight: weight != null ? Number(weight) : undefined,
        rpm: rpm != null ? Number(rpm) : undefined,
        daily_max: daily != null ? Number(daily) : undefined,
        proxy_id: proxyId,
        groups: groupPicked ?? q.data?.account.groups ?? [],
      }),
    onSuccess: () => {
      toast.success("已保存");
      void qc.invalidateQueries({ queryKey: ["account", decoded] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const refreshUsage = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      const res = await adminApi.refreshAccountUsage(decoded);
      await q.refetch();
      if (res.usage_error) toast.error(res.usage_error);
      else toast.success("用量已刷新");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "刷新失败");
    } finally {
      setRefreshing(false);
    }
  };

  if (q.isError) return <p className="text-crimson">找不到账号，或尚未入池（浏览器登录完成前）。</p>;
  if (!q.data) return <p className="text-ash">加载账号…</p>;
  const a = q.data.account;
  return (
    <div>
      <PageHeader
        title={a.email || a.name}
        description={
          a.email && a.email !== a.name
            ? `${a.name} · 调度字段、出口绑定，以及该号经手的请求头体。`
            : "调度字段、出口绑定，以及该号经手的请求头体。"
        }
        actions={
          <>
            <Button variant="secondary" disabled={refreshing} onClick={() => void refreshUsage()}>
              <RefreshCw className={cn(refreshing && "animate-spin")} />
              {refreshing ? "正在刷新…" : "刷新用量"}
            </Button>
            <Button
              variant="destructive"
              onClick={async () => {
                await adminApi.deleteAccount(decoded);
                nav("/accounts");
              }}
            >
              删除
            </Button>
          </>
        }
      />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <div className="flex items-center justify-between">
            <span className="font-mono text-[12px] text-ash">启用</span>
            <Switch
              checked={a.enabled}
              onCheckedChange={async (enabled) => {
                await adminApi.patchAccount({ name: decoded, enabled });
                void q.refetch();
              }}
            />
          </div>
          <div className="mt-4 grid gap-3 sm:grid-cols-2">
            <Field label="优先级" value={prio ?? String(a.priority ?? 0)} onChange={setPrio} />
            <Field label="权重" value={weight ?? String(a.weight ?? 1)} onChange={setWeight} />
            <Field label="RPM" value={rpm ?? String(a.rpm ?? 0)} onChange={setRpm} />
            <Field label="日限" value={daily ?? String(a.daily_max ?? 0)} onChange={setDaily} />
          </div>
          <div className="mt-3 space-y-1.5">
            <Label>分组</Label>
            <GroupChecks
              value={groupPicked ?? a.groups ?? []}
              groups={groups.data?.groups ?? []}
              extras={a.groups}
              onChange={setGroupPicked}
            />
          </div>
          <div className="mt-3 space-y-1.5">
            <Label>出口代理</Label>
            <select
              className="h-9 w-full rounded-md border border-border bg-bone px-3 text-[14px]"
              value={proxyId ?? a.proxy_id ?? ""}
              onChange={(e) => setProxyId(e.target.value)}
            >
              <option value="">默认 / 全局</option>
              {proxies.data?.proxies.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name} ({p.kind})
                </option>
              ))}
            </select>
          </div>
          <Button className="mt-4" onClick={() => save.mutate()}>
            保存调度字段
          </Button>
          <div className="mt-4 flex flex-wrap gap-3 font-mono text-[12px] text-ash">
            <Badge tone={a.logged_in ? "ok" : "warn"}>{a.logged_in ? "已登录" : "未登录"}</Badge>
            <Badge tone={a.plan_label?.toLowerCase().includes("free") ? "muted" : "ember"}>
              {a.plan_label || a.plan_badge || "套餐未同步"}
            </Badge>
            <span>失败分类 {a.fail_class || "—"}</span>
            <span>冷却至 {formatTime(a.cooldown_until)}</span>
            <span>停用原因 {a.disable_reason ? formatDisableReason(a.disable_reason) : "—"}</span>
          </div>
        </Card>
        <Card>
          <div className="font-mono text-[12px] text-ash">官方用量</div>
          <div className="mt-3">
            <UsageMeters
              usage={
                hasUsageMeters(usageFromAccount(a))
                  ? usageFromAccount(a)
                  : usageFromVendor(q.data.usage.vendor, q.data.usage.usage_error || q.data.usage.error)
              }
              onRefresh={refreshUsage}
              refreshing={refreshing}
            />
          </div>
          <div className="mt-3">
            <JsonView value={q.data.usage} />
          </div>
          <div className="mt-3 font-mono text-[12px] text-ash">当前出口</div>
          <JsonView value={q.data.egress} />
        </Card>
      </div>
      <Card className="mt-4">
        <div className="mb-3 font-mono text-[12px] text-ash">请求记录（含头与体，敏感字段已脱敏）</div>
        <div className="space-y-4">
          {q.data.logs.map((l) => (
            <div key={l.id} className="border-b border-border/70 pb-4 last:border-0">
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <Link to={`/logs/${l.id}`} className="font-mono text-[12px] text-ember hover:underline">
                  {l.method} {l.path}
                </Link>
                <span className="font-mono text-[12px] text-ash">
                  {l.status} · {l.latency_ms}ms · {formatTime(l.time)}
                </span>
              </div>
              <div className="mt-2 grid gap-2 lg:grid-cols-2">
                <div>
                  <div className="mb-1 font-mono text-[11px] text-ash">请求头</div>
                  <JsonView value={l.headers ?? {}} />
                </div>
                <div>
                  <div className="mb-1 font-mono text-[11px] text-ash">请求体</div>
                  <JsonView value={l.request_body || "—"} />
                </div>
              </div>
            </div>
          ))}
          {q.data.logs.length === 0 ? <p className="font-serif text-driftwood">还没有经手请求。</p> : null}
        </div>
      </Card>
    </div>
  );
}

function Field({ label, value, onChange }: { label: string; value: string; onChange: (v: string) => void }) {
  return (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      <Input value={value} onChange={(e) => onChange(e.target.value)} />
    </div>
  );
}
