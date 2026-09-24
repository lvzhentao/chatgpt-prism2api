import { useQuery } from "@tanstack/react-query";
import ReactECharts from "echarts-for-react";
import { Link } from "react-router-dom";
import { Card } from "@/components/ui/card";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { Badge } from "@/components/ui/badge";
import { PageHeader } from "@/components/shared/page-header";
import { baseOption, chartColors } from "@/components/shared/chart-theme";
import { adminApi } from "@/lib/api";
import { formatDuration, formatNumber, formatPercent, formatTime } from "@/lib/format";
import { useThemeStore } from "@/stores/theme";

function mapToPairs(m?: Record<string, number>) {
  return Object.entries(m ?? {})
    .sort((a, b) => b[1] - a[1])
    .slice(0, 8);
}

export function OverviewPage() {
  const dark = useThemeStore((s) => s.theme) === "dark";
  const q = useQuery({ queryKey: ["dashboard"], queryFn: adminApi.dashboard, refetchInterval: 15000 });
  if (q.isLoading) return <PageSkeleton />;
  if (q.isError || !q.data) return <p className="text-crimson">无法加载总览</p>;
  const d = q.data;
  const c = chartColors(dark);
  const modelPairs = mapToPairs(d.stats.by_model);
  const accPairs = mapToPairs(d.stats.by_account);
  const failPairs = mapToPairs(d.stats.by_fail_class);

  return (
    <div>
      <PageHeader title="工作台" description="池子是否就绪、错误从哪来、哪些号在冷却——先看这里再动手。" />
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        {[
          ["就绪", d.accounts.ready, "ok"],
          ["冷却", d.accounts.cooling, "warn"],
          ["停用", d.accounts.disabled, "muted"],
          ["账号合计", d.accounts.total, "ember"],
        ].map(([k, v, tone]) => (
          <Card key={String(k)} className="p-4">
            <div className="font-mono text-[12px] text-ash">{k}</div>
            <div className="mt-2 text-[36px] leading-none tracking-[-0.72px] text-ink">{v}</div>
            <Badge className="mt-2" tone={tone as "ok"}>
              {k === "账号合计" ? `运行 ${formatDuration(d.uptime_seconds)}` : "实时"}
            </Badge>
          </Card>
        ))}
      </div>

      <div className="mt-6 grid gap-4 lg:grid-cols-2">
        <Card>
          <div className="mb-2 font-mono text-[12px] text-ash">请求与错误</div>
          <p className="text-[22px] tracking-[-0.11px] text-ink">
            {formatNumber(d.stats.total_requests)} 次 · 错误率 {formatPercent(d.stats.error_rate)}
          </p>
          <p className="mt-1 text-[13px] text-ash">
            Token {formatNumber(d.stats.total_tokens)} · 均延迟 {d.stats.avg_latency_ms} ms
          </p>
          <ReactECharts
            style={{ height: 240 }}
            option={{
              ...baseOption(dark),
              series: [
                {
                  type: "pie",
                  radius: ["46%", "68%"],
                  label: { color: c.ash },
                  data: [
                    { name: "成功", value: d.stats.total_requests - d.stats.error_requests, itemStyle: { color: c.forest } },
                    { name: "错误", value: d.stats.error_requests, itemStyle: { color: c.crimson } },
                  ],
                },
              ],
            }}
          />
        </Card>
        <Card>
          <div className="mb-2 font-mono text-[12px] text-ash">模型调用</div>
          <ReactECharts
            style={{ height: 280 }}
            option={{
              ...baseOption(dark),
              xAxis: { type: "value", axisLine: { lineStyle: { color: c.stone } } },
              yAxis: {
                type: "category",
                data: modelPairs.map(([k]) => k).reverse(),
                axisLabel: { color: c.ash, width: 120, overflow: "truncate" },
              },
              series: [
                {
                  type: "bar",
                  data: modelPairs.map(([, v]) => v).reverse(),
                  itemStyle: { color: c.ink, borderRadius: 2 },
                },
              ],
            }}
          />
        </Card>
        <Card>
          <div className="mb-2 font-mono text-[12px] text-ash">账号吞吐</div>
          <ReactECharts
            style={{ height: 260 }}
            option={{
              ...baseOption(dark),
              xAxis: {
                type: "category",
                data: accPairs.map(([k]) => k),
                axisLabel: { color: c.ash, rotate: 30 },
              },
              yAxis: { type: "value", splitLine: { lineStyle: { color: c.stone } } },
              series: [{ type: "bar", data: accPairs.map(([, v]) => v), itemStyle: { color: c.amber } }],
            }}
          />
        </Card>
        <Card>
          <div className="mb-2 font-mono text-[12px] text-ash">失败分类</div>
          <ReactECharts
            style={{ height: 260 }}
            option={{
              ...baseOption(dark),
              series: [
                {
                  type: "pie",
                  radius: "68%",
                  data: failPairs.map(([name, value], i) => ({
                    name,
                    value,
                    itemStyle: { color: [c.crimson, c.amber, c.ember, c.forest, c.ink][i % 5] },
                  })),
                },
              ],
            }}
          />
        </Card>
      </div>

      <Card className="mt-4">
        <div className="mb-3 font-mono text-[12px] text-ash">冷却中</div>
        {d.cooldowns.length === 0 ? (
          <p className="font-serif text-[16px] text-driftwood">没有账号在冷却。</p>
        ) : (
          <ul className="space-y-2">
            {d.cooldowns.map((c) => (
              <li key={c.name} className="flex flex-wrap items-baseline justify-between gap-2 border-b border-border/60 py-2 last:border-0">
                <Link to={`/accounts/${c.name}`} className="text-ember hover:underline">
                  {c.name}
                </Link>
                <span className="font-mono text-[12px] text-ash">
                  {c.fail_class || c.reason} · 至 {formatTime(c.until)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}
