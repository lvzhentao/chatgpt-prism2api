import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardDesc, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { adminApi } from "@/lib/api";
import type { EmulationConfig, TavilyKey } from "@/types/api";

function emptyConfig(): EmulationConfig {
  return {
    cache: {
      enabled: true,
      mode: "uniform",
      ratio: 1,
      ratio_min: 1,
      ratio_max: 1,
      creation_ratio: 1,
      read_ratio: 1,
      read_ratio_min: 1,
      read_ratio_max: 1,
      force_hit: false,
      min_tokens: 1024,
      opus_min_tokens: 4096,
      fallback_breakpoints: true,
      ttl_5m_seconds: 300,
      ttl_1h_seconds: 3600,
    },
    web_search: { enabled: true, provider: "auto", max_results: 5, tavily_keys: [] },
    signature: { enabled: true },
  };
}

export function EmulationPage() {
  const q = useQuery({ queryKey: ["emulation"], queryFn: adminApi.emulation });
  const [draft, setDraft] = useState<EmulationConfig | null>(null);
  const [newKey, setNewKey] = useState("");
  const [newName, setNewName] = useState("");
  const [testQuery, setTestQuery] = useState("Anthropic Claude web search");
  const cfg = draft ?? q.data?.config ?? emptyConfig();

  useEffect(() => {
    if (q.data?.config && !draft) setDraft(q.data.config);
  }, [q.data, draft]);

  const save = useMutation({
    mutationFn: () => adminApi.updateEmulation(cfg),
    onSuccess: (res) => {
      toast.success("协议外观已保存");
      setDraft(res.config);
      void q.refetch();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const test = useMutation({
    mutationFn: () => adminApi.testWebSearch(testQuery),
    onSuccess: (res) => {
      toast.success(`${res.provider} 返回 ${res.results?.length ?? 0} 条（${res.latency_ms}ms）`);
      void q.refetch();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const patchCache = (p: Partial<EmulationConfig["cache"]>) =>
    setDraft({ ...cfg, cache: { ...cfg.cache, ...p } });
  const patchWS = (p: Partial<EmulationConfig["web_search"]>) =>
    setDraft({ ...cfg, web_search: { ...cfg.web_search, ...p } });

  const addKey = () => {
    const key = newKey.trim();
    if (!key) return;
    const item: TavilyKey = {
      id: `tvk_${Date.now()}`,
      name: newName.trim() || `key-${(cfg.web_search.tavily_keys?.length ?? 0) + 1}`,
      key,
      enabled: true,
    };
    patchWS({ tavily_keys: [...(cfg.web_search.tavily_keys ?? []), item] });
    setNewKey("");
    setNewName("");
  };

  if (q.isLoading) return <PageSkeleton />;

  return (
    <div className="space-y-6">
      <PageHeader
        title="协议外观"
        description="缓存拆分、thinking 签名、web_search 官方块。搜索先走上游账号池，失败再轮询 Tavily key 池。"
        actions={
          <Button onClick={() => save.mutate()} disabled={save.isPending}>
            保存
          </Button>
        }
      />

      <Card>
        <div className="flex items-center justify-between gap-4">
          <div>
            <CardTitle>提示词缓存</CardTitle>
            <CardDesc>
              对齐参考项目：input + cache_read + cache_creation 恒等于请求总量。倍率只缩放 read/create，剩余记进 input。
            </CardDesc>
          </div>
          <Switch checked={cfg.cache.enabled} onCheckedChange={(v) => patchCache({ enabled: v })} />
        </div>
        <div className="mt-5 flex gap-1 rounded-md border border-border bg-linen/50 p-1">
          {(["uniform", "independent"] as const).map((mode) => (
            <button
              key={mode}
              type="button"
              className={`h-9 flex-1 rounded-sm text-[13px] ${cfg.cache.mode === mode ? "bg-bone text-ink" : "text-ash"}`}
              onClick={() =>
                patchCache(
                  mode === "uniform"
                    ? {
                        mode,
                        ratio: cfg.cache.read_ratio,
                        ratio_min: cfg.cache.read_ratio_min ?? cfg.cache.read_ratio,
                        ratio_max: cfg.cache.read_ratio_max ?? cfg.cache.read_ratio,
                      }
                    : {
                        mode,
                        read_ratio: cfg.cache.ratio,
                        read_ratio_min: cfg.cache.ratio_min ?? cfg.cache.ratio,
                        read_ratio_max: cfg.cache.ratio_max ?? cfg.cache.ratio,
                      },
                )
              }
            >
              {mode === "uniform" ? "统一倍率" : "读写分开"}
            </button>
          ))}
        </div>
        <div className="mt-5 space-y-4">
          {cfg.cache.mode === "uniform" ? (
            <RatioRange
              label="缓存倍率区间"
              hint="每笔请求在下限~上限内随机采样；1 = 官方拆分；0.5 = 只报一半缓存，另一半算普通 input"
              lo={cfg.cache.ratio_min ?? cfg.cache.ratio}
              hi={cfg.cache.ratio_max ?? cfg.cache.ratio}
              onChange={(lo, hi) => patchCache({ ratio_min: lo, ratio_max: hi, ratio: hi })}
            />
          ) : (
            <>
              <RatioSlider label="写入倍率" hint="首轮 cache_creation 占可缓存前缀的比例" value={cfg.cache.creation_ratio} onChange={(v) => patchCache({ creation_ratio: v })} />
              <RatioRange
                label="读取倍率区间"
                hint="命中后每笔请求在下限~上限内随机采样 cache_read 占比"
                lo={cfg.cache.read_ratio_min ?? cfg.cache.read_ratio}
                hi={cfg.cache.read_ratio_max ?? cfg.cache.read_ratio}
                onChange={(lo, hi) => patchCache({ read_ratio_min: lo, read_ratio_max: hi, read_ratio: hi })}
              />
            </>
          )}
        </div>
        <label className="mt-4 flex items-center justify-between gap-4 border-t border-border/60 pt-4">
          <div>
            <div className="text-[14px] text-ink">强制命中</div>
            <div className="font-mono text-[11px] text-ash">跳过前缀匹配，所有请求（含首轮）直接按读取区间报 cache_read。首轮即 read 不符合官方行为，谨慎开</div>
          </div>
          <Switch checked={!!cfg.cache.force_hit} onCheckedChange={(v) => patchCache({ force_hit: v })} />
        </label>
        <CachePreview cache={cfg.cache} />
        <div className="mt-5 grid gap-4 sm:grid-cols-2">
          <NumField label="最小 token" value={cfg.cache.min_tokens} onChange={(v) => patchCache({ min_tokens: v })} />
          <NumField label="Opus 最小 token" value={cfg.cache.opus_min_tokens} onChange={(v) => patchCache({ opus_min_tokens: v })} />
        </div>
        <label className="mt-4 flex items-center justify-between gap-4 border-t border-border/60 pt-4">
          <div>
            <div className="text-[14px] text-ink">无 cache_control 时兜底断点</div>
            <div className="font-mono text-[11px] text-ash">每个消息末尾 + 最后一块，默认 5 分钟</div>
          </div>
          <Switch checked={cfg.cache.fallback_breakpoints} onCheckedChange={(v) => patchCache({ fallback_breakpoints: v })} />
        </label>
      </Card>

      <Card>
        <div className="flex items-center justify-between gap-4">
          <div>
            <CardTitle>Web Search</CardTitle>
            <CardDesc>只拦截「仅有一个 web_search 工具」的检测站请求。Claude Code 多工具路径不改。</CardDesc>
          </div>
          <Switch checked={cfg.web_search.enabled} onCheckedChange={(v) => patchWS({ enabled: v })} />
        </div>
        <div className="mt-5 grid gap-4 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label>后端</Label>
            <select
              className="flex h-9 w-full rounded-md border border-border bg-bone px-3 text-[14px]"
              value={cfg.web_search.provider}
              onChange={(e) => patchWS({ provider: e.target.value as EmulationConfig["web_search"]["provider"] })}
            >
              <option value="auto">auto（上游优先，失败走 Tavily）</option>
              <option value="vendor">仅上游</option>
              <option value="tavily">仅 Tavily 池</option>
            </select>
          </div>
          <NumField label="最多结果" value={cfg.web_search.max_results} onChange={(v) => patchWS({ max_results: v })} />
        </div>

        <div className="mt-5 space-y-2">
          <Label>Tavily key 池</Label>
          <div className="flex flex-wrap gap-2">
            <Input placeholder="备注" className="w-36" value={newName} onChange={(e) => setNewName(e.target.value)} />
            <Input
              placeholder="tvly-..."
              type="password"
              className="min-w-[220px] flex-1"
              value={newKey}
              onChange={(e) => setNewKey(e.target.value)}
            />
            <Button type="button" variant="secondary" onClick={addKey}>
              加入池
            </Button>
          </div>
          {(cfg.web_search.tavily_keys ?? []).length === 0 ? (
            <p className="font-serif text-[15px] text-driftwood">还没有 Tavily key。上游搜不到时会落到这里。</p>
          ) : (
            <Table>
              <THead>
                <TR>
                  <TH>名称</TH>
                  <TH>Key</TH>
                  <TH>启用</TH>
                  <TH>最近</TH>
                  <TH />
                </TR>
              </THead>
              <TBody>
                {(cfg.web_search.tavily_keys ?? []).map((k) => (
                  <TR key={k.id}>
                    <TD>{k.name || k.id}</TD>
                    <TD className="font-mono text-[12px]">{k.key || "••••"}</TD>
                    <TD>
                      <Switch
                        checked={k.enabled}
                        onCheckedChange={(v) =>
                          patchWS({
                            tavily_keys: (cfg.web_search.tavily_keys ?? []).map((x) => (x.id === k.id ? { ...x, enabled: v } : x)),
                          })
                        }
                      />
                    </TD>
                    <TD className="max-w-[220px] truncate font-mono text-[11px] text-ash">{k.last_error || k.last_used_at || "—"}</TD>
                    <TD>
                      <Button
                        variant="ghost"
                        onClick={() => patchWS({ tavily_keys: (cfg.web_search.tavily_keys ?? []).filter((x) => x.id !== k.id) })}
                      >
                        删除
                      </Button>
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          )}
        </div>

        <div className="mt-5 flex flex-wrap gap-2 border-t border-border/60 pt-4">
          <Input className="min-w-[240px] flex-1" value={testQuery} onChange={(e) => setTestQuery(e.target.value)} />
          <Button type="button" variant="secondary" disabled={test.isPending} onClick={() => test.mutate()}>
            试搜
          </Button>
        </div>
      </Card>

      <Card>
        <div className="flex items-center justify-between gap-4">
          <div>
            <CardTitle>Thinking 签名</CardTitle>
            <CardDesc>在 thinking 块结束前发 signature_delta，非流式带 signature 字段。</CardDesc>
          </div>
          <Switch
            checked={cfg.signature.enabled}
            onCheckedChange={(v) => setDraft({ ...cfg, signature: { enabled: v } })}
          />
        </div>
      </Card>

      <Card className="p-0">
        <div className="border-b border-border/60 px-6 py-4">
          <CardTitle>最近缓存</CardTitle>
          <CardDesc>用来对比例和命中。只留进程内最近 20 条。</CardDesc>
        </div>
        {(q.data?.cache_log ?? []).length === 0 ? (
          <p className="px-6 py-8 font-serif text-[15px] text-driftwood">还没有缓存估算。</p>
        ) : (
          <Table>
            <THead>
              <TR>
                <TH>时间</TH>
                <TH>模型</TH>
                <TH>总量</TH>
                <TH>input</TH>
                <TH>read</TH>
                <TH>read%</TH>
                <TH>create</TH>
                <TH>命中</TH>
              </TR>
            </THead>
            <TBody>
              {(q.data?.cache_log ?? []).map((row, i) => {
                const total = row.total ?? row.input + row.read + row.creation;
                return (
                  <TR key={`${row.time}-${i}`}>
                    <TD className="font-mono text-[12px]">{row.time}</TD>
                    <TD className="font-mono text-[12px]">{row.model}</TD>
                    <TD>{total}</TD>
                    <TD>{row.input}</TD>
                    <TD>{row.read}</TD>
                    <TD className="font-mono text-[12px]">{total > 0 ? `${Math.round((row.read / total) * 100)}%` : "—"}</TD>
                    <TD>{row.creation}</TD>
                    <TD>{row.hit ? <Badge tone="ok">hit</Badge> : "—"}</TD>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        )}
      </Card>

      <Card className="p-0">
        <div className="border-b border-border/60 px-6 py-4">
          <CardTitle>最近搜索</CardTitle>
        </div>
        {(q.data?.search_log ?? []).length === 0 ? (
          <p className="px-6 py-8 font-serif text-[15px] text-driftwood">还没有 web_search 拦截。</p>
        ) : (
          <Table>
            <THead>
              <TR>
                <TH>时间</TH>
                <TH>查询</TH>
                <TH>后端</TH>
                <TH>条数</TH>
                <TH>耗时</TH>
                <TH>错误</TH>
              </TR>
            </THead>
            <TBody>
              {(q.data?.search_log ?? []).map((row, i) => (
                <TR key={`${row.time}-${i}`}>
                  <TD className="font-mono text-[12px]">{row.time}</TD>
                  <TD className="max-w-[280px] truncate">{row.query}</TD>
                  <TD className="font-mono text-[12px]">{row.provider}</TD>
                  <TD>{row.results}</TD>
                  <TD>{row.latency_ms}ms</TD>
                  <TD className="max-w-[200px] truncate text-ash">{row.error || "—"}</TD>
                </TR>
              ))}
            </TBody>
          </Table>
        )}
      </Card>
    </div>
  );
}

function scaleTokens(tokens: number, ratio: number) {
  if (tokens <= 0 || ratio <= 0) return 0;
  if (ratio >= 1) return tokens;
  return Math.round(tokens * ratio);
}

function rangeStr(lo: number, hi: number) {
  return lo === hi ? `${lo}` : `${lo} ~ ${hi}`;
}

function previewSplit(cache: EmulationConfig["cache"], total = 20000) {
  const uniformLo = cache.ratio_min ?? cache.ratio;
  const uniformHi = cache.ratio_max ?? cache.ratio;
  const readLo = scaleTokens(total, cache.mode === "independent" ? cache.read_ratio_min ?? cache.read_ratio : uniformLo);
  const readHi = scaleTokens(total, cache.mode === "independent" ? cache.read_ratio_max ?? cache.read_ratio : uniformHi);
  const createLo = scaleTokens(total, cache.mode === "independent" ? cache.creation_ratio : uniformLo);
  const createHi = scaleTokens(total, cache.mode === "independent" ? cache.creation_ratio : uniformHi);
  if (cache.force_hit) {
    return {
      first: { readLo, readHi, createLo: 0, createHi: 0 },
      second: { readLo, readHi, createLo: 0, createHi: 0 },
    };
  }
  return {
    first: { readLo: 0, readHi: 0, createLo, createHi },
    second: { readLo, readHi, createLo: 0, createHi: 0 },
  };
}

function RatioRange({
  label,
  hint,
  lo,
  hi,
  onChange,
}: {
  label: string;
  hint?: string;
  lo: number;
  hi: number;
  onChange: (lo: number, hi: number) => void;
}) {
  const pctLo = Math.round(Math.min(1, Math.max(0, lo)) * 100);
  const pctHi = Math.round(Math.min(1, Math.max(0, hi)) * 100);
  const setLo = (v: number) => onChange(Math.min(v / 100, hi), Math.max(v / 100, hi));
  const setHi = (v: number) => onChange(Math.min(v / 100, lo), Math.max(v / 100, lo));
  return (
    <div className="space-y-1.5">
      <div className="flex items-end justify-between">
        <Label>{label}</Label>
        <span className="font-mono text-[13px] text-ink">{pctLo}% ~ {pctHi}%</span>
      </div>
      {hint ? <p className="text-[12px] text-ash">{hint}</p> : null}
      <div className="grid grid-cols-2 gap-2">
        <NumField label="下限 %" value={pctLo} onChange={setLo} />
        <NumField label="上限 %" value={pctHi} onChange={setHi} />
      </div>
    </div>
  );
}

function RatioSlider({
  label,
  hint,
  value,
  onChange,
}: {
  label: string;
  hint?: string;
  value: number;
  onChange: (v: number) => void;
}) {
  const pct = Math.round(Math.min(1, Math.max(0, value)) * 100);
  return (
    <div className="space-y-1.5">
      <div className="flex items-end justify-between">
        <Label>{label}</Label>
        <span className="font-mono text-[13px] text-ink">{pct}%</span>
      </div>
      {hint ? <p className="text-[12px] text-ash">{hint}</p> : null}
      <input
        type="range"
        min={0}
        max={100}
        step={1}
        value={pct}
        onChange={(e) => onChange(Number(e.target.value) / 100)}
        className="w-full accent-current"
      />
    </div>
  );
}

function CachePreview({ cache }: { cache: EmulationConfig["cache"] }) {
  const total = 20000;
  const { first, second } = previewSplit(cache, total);
  const inputStr = (readLo: number, readHi: number, createLo: number, createHi: number) =>
    rangeStr(total - readHi - createHi, total - readLo - createLo);
  return (
    <div className="mt-5 rounded-md border border-border/70 bg-linen/40 p-4">
      <div className="text-[13px] text-ink">预览 · 整段 20000 token 可缓存前缀</div>
      <p className="mt-1 text-[12px] text-ash">input + read + create = 20000。区间内逐请求随机采样，检测站看的是这三项，不是只看 input。</p>
      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <PreviewCard
          title={cache.force_hit ? "第 1 轮（强制命中）" : "第 1 轮（写入）"}
          input={inputStr(first.readLo, first.readHi, first.createLo, first.createHi)}
          read={rangeStr(first.readLo, first.readHi)}
          create={rangeStr(first.createLo, first.createHi)}
        />
        <PreviewCard
          title="第 2 轮（命中）"
          input={inputStr(second.readLo, second.readHi, second.createLo, second.createHi)}
          read={rangeStr(second.readLo, second.readHi)}
          create={rangeStr(second.createLo, second.createHi)}
        />
      </div>
    </div>
  );
}

function PreviewCard({ title, input, read, create }: { title: string; input: string; read: string; create: string }) {
  return (
    <div>
      <div className="text-[12px] text-ash">{title}</div>
      <div className="mt-1 font-mono text-[12px] text-ink">
        input {input} · read {read} · create {create}
      </div>
    </div>
  );
}

function NumField({
  label,
  value,
  step,
  onChange,
}: {
  label: string;
  value: number;
  step?: string;
  onChange: (v: number) => void;
}) {
  return (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      <Input
        type="number"
        step={step}
        value={Number.isFinite(value) ? value : 0}
        onChange={(e) => onChange(Number(e.target.value))}
      />
    </div>
  );
}
