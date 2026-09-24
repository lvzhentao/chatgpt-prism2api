import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { RotateCcw } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { EmptyState } from "@/components/shared/empty-state";
import { ModelLogo } from "@/components/shared/model-logo";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton, Stagger } from "@/components/shared/page-skeleton";
import { RoutePicker } from "@/components/shared/route-picker";
import { adminApi } from "@/lib/api";
import { vendorOf, vendorSort } from "@/lib/model-meta";
import { cn } from "@/lib/utils";
import type { ModelRow } from "@/types/api";

const PAGE = 24;

function asRoutes(raw: unknown): Record<string, string> {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return {};
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof v === "string" && v.trim()) out[k] = v.trim();
  }
  return out;
}

export function ModelsPage() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const models = useQuery({ queryKey: ["models"], queryFn: adminApi.models });
  const cfg = useQuery({ queryKey: ["config"], queryFn: adminApi.config });
  const saved = asRoutes(cfg.data?.model_routes);
  const [draft, setDraft] = useState<Record<string, string> | null>(null);
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [q, setQ] = useState("");
  const [vendor, setVendor] = useState("");
  const [limit, setLimit] = useState(PAGE);
  const moreRef = useRef<HTMLDivElement>(null);
  const routes = draft ?? saved;
  const rows = models.data?.models ?? [];
  const ids = useMemo(() => rows.map((m) => m.id), [rows]);

  const grouped = useMemo(() => {
    const map = new Map<string, { label: string; items: ModelRow[] }>();
    for (const m of rows) {
      const v = vendorOf(m.id);
      const g = map.get(v.id) ?? { label: v.label, items: [] };
      g.items.push(m);
      map.set(v.id, g);
    }
    return [...map.entries()].sort((a, b) => vendorSort(a[0], b[0]));
  }, [rows]);

  useEffect(() => {
    if (!vendor && grouped[0]) setVendor(grouped[0][0]);
  }, [grouped, vendor]);

  useEffect(() => {
    setLimit(PAGE);
  }, [vendor, q]);

  const active = grouped.find(([id]) => id === vendor);
  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const items = active?.[1].items ?? [];
    if (!needle) return items;
    const vlabel = active?.[1].label ?? "";
    return items.filter((m) => `${m.id} ${m.display_name} ${vlabel}`.toLowerCase().includes(needle));
  }, [active, q]);
  const visible = filtered.slice(0, limit);

  useEffect(() => {
    const el = moreRef.current;
    if (!el || visible.length >= filtered.length) return;
    const io = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) setLimit((n) => n + PAGE);
    }, { rootMargin: "160px" });
    io.observe(el);
    return () => io.disconnect();
  }, [visible.length, filtered.length, vendor]);

  const save = useMutation({
    mutationFn: (next: Record<string, string>) => adminApi.updateConfig({ model_routes: next }),
    onSuccess: () => {
      toast.success("路由已保存");
      setDraft(null);
      void qc.invalidateQueries({ queryKey: ["config"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const setRoute = (source: string, target: string) => {
    const next = { ...routes };
    if (!target) delete next[source];
    else next[source] = target;
    setDraft(next);
  };

  const dirty = draft != null && JSON.stringify(draft) !== JSON.stringify(saved);

  if (models.isLoading) {
    return (
      <div>
        <PageHeader title="模型目录" description="按厂家切换，当前页才渲染模型行。" />
        <PageSkeleton kind="models" />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="模型目录"
        description="按厂家分 Tab，当前厂家才挂载列表。Logo 来自 LobeHub。路由例如 claude-fable-5 → claude-opus-4-8，空则透传。"
        actions={
          <>
            <Button
              variant="outline"
              onClick={() => {
                if (!window.confirm("清除全部自定义路由，恢复为默认透传？")) return;
                setDraft({});
                save.mutate({});
              }}
            >
              <RotateCcw /> 重置全部默认
            </Button>
            <Button
              onClick={async () => {
                const t = await adminApi.modelsRefreshTask();
                toast.success("已排队");
                nav(`/tasks/${t.task.id}`);
              }}
            >
              刷新上游
            </Button>
            <Button disabled={!dirty || save.isPending} onClick={() => save.mutate(routes)}>
              保存路由
            </Button>
          </>
        }
      />

      <Card className="mb-5 max-w-3xl">
        <div className="font-mono text-[12px] text-ash">自定义一条路由</div>
        <div className="mt-3 grid gap-2 sm:grid-cols-[1fr_auto_1fr_auto] sm:items-end">
          <div className="space-y-1">
            <div className="text-[12px] text-ash">来源模型</div>
            <Input value={from} onChange={(e) => setFrom(e.target.value)} placeholder="claude-fable-5" className="font-mono text-[13px]" />
          </div>
          <div className="hidden pb-2 text-ash sm:block">→</div>
          <div className="space-y-1">
            <div className="text-[12px] text-ash">路由到</div>
            <Input value={to} onChange={(e) => setTo(e.target.value)} placeholder="claude-opus-4-8" className="font-mono text-[13px]" />
          </div>
          <Button
            variant="secondary"
            onClick={() => {
              if (!from.trim() || !to.trim()) return;
              setRoute(from.trim(), to.trim());
              setFrom("");
              setTo("");
            }}
          >
            加入草稿
          </Button>
        </div>
        {Object.keys(routes).length > 0 ? (
          <div className="mt-4 space-y-2">
            {Object.entries(routes).map(([src, dst]) => (
              <div key={src} className="flex flex-wrap items-center gap-2 font-mono text-[12px]">
                <span className="text-ink">{src}</span>
                <span className="text-ash">→</span>
                <span className="text-ember">{dst}</span>
                <Button size="sm" variant="ghost" onClick={() => setRoute(src, "")}>
                  恢复默认
                </Button>
              </div>
            ))}
          </div>
        ) : (
          <p className="mt-3 font-serif text-[15px] text-driftwood">当前全部透传，没有覆盖项。</p>
        )}
      </Card>

      {rows.length === 0 ? (
        <EmptyState title="目录还是空的。点刷新上游会排进任务中心。" />
      ) : (
        <>
          <div className="mb-3 flex gap-1 overflow-x-auto border-b border-border pb-px">
            {grouped.map(([id, g]) => (
              <button
                key={id}
                type="button"
                onClick={() => setVendor(id)}
                className={cn(
                  "inline-flex shrink-0 items-center gap-1.5 px-3 py-2 text-[13px] text-ash transition-colors",
                  vendor === id && "text-ink shadow-[inset_0_-1px_0_0_var(--color-ink)]",
                )}
              >
                <ModelLogo id={g.items[0]?.id || id} className="size-5" />
                {g.label}
                <span className="font-mono text-[11px]">{g.items.length}</span>
              </button>
            ))}
          </div>
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder={`在 ${active?.[1].label || "当前厂家"} 中过滤`}
            className="mb-4 max-w-md"
          />
          {visible.length === 0 ? (
            <EmptyState title="这一家里没有匹配的模型。" />
          ) : (
            <div key={vendor} className="page-enter grid gap-2">
              {visible.map((m, i) => {
                const mapped = routes[m.id] || routes[m.id.toLowerCase()] || "";
                return (
                  <Stagger key={m.id} i={Math.min(i, 12)}>
                    <Card interactive className="flex flex-col gap-2 p-3 sm:flex-row sm:items-center">
                      <div className="flex min-w-0 flex-1 items-center gap-3">
                        <ModelLogo id={m.id} />
                        <div className="min-w-0">
                          <div className="truncate text-[14px] text-ink">{m.display_name || m.id}</div>
                          <div className="truncate font-mono text-[11px] text-ash">{m.id}</div>
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-2">
                        {m.supports_thinking ? <Badge tone="ember">思考</Badge> : null}
                        {m.supports_images ? <Badge>图像</Badge> : null}
                        {m.supports_agent ? <Badge tone="ok">Agent</Badge> : null}
                        <span className="font-mono text-[11px] text-ash">{m.context_token_limit || "—"}</span>
                      </div>
                      <div className="flex w-full items-center gap-2 sm:w-[260px]">
                        <RoutePicker value={mapped} options={ids} onChange={(v) => setRoute(m.id, v)} />
                        {mapped ? (
                          <Button size="icon" variant="ghost" aria-label="恢复默认" onClick={() => setRoute(m.id, "")}>
                            <RotateCcw />
                          </Button>
                        ) : null}
                      </div>
                    </Card>
                  </Stagger>
                );
              })}
              {visible.length < filtered.length ? (
                <div ref={moreRef} className="py-3 text-center font-mono text-[12px] text-ash">
                  继续下滑加载 · 已显示 {visible.length}/{filtered.length}
                </div>
              ) : null}
            </div>
          )}
        </>
      )}
    </div>
  );
}
