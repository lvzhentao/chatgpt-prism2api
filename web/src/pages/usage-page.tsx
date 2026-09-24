import { useQuery } from "@tanstack/react-query";
import ReactECharts from "echarts-for-react";
import { Link, useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { baseOption, chartColors } from "@/components/shared/chart-theme";
import { adminApi } from "@/lib/api";
import { formatNumber, formatTime, formatUSDFromCents } from "@/lib/format";
import { useThemeStore } from "@/stores/theme";

export function UsagePage() {
  const nav = useNavigate();
  const dark = useThemeStore((s) => s.theme) === "dark";
  const q = useQuery({ queryKey: ["usage"], queryFn: () => adminApi.usage() });
  const c = chartColors(dark);
  const rows = q.data?.accounts ?? [];
  return (
    <div>
      <PageHeader
        title="用量"
        description="本地成功次数与 上游官方配额缓存。全量刷新会进入任务中心。"
        actions={
          <Button
            onClick={async () => {
              const t = await adminApi.usageRefreshTask();
              toast.success("已排队");
              nav(`/tasks/${t.task.id}`);
            }}
          >
            刷新全部
          </Button>
        }
      />
      {q.isLoading ? (
        <PageSkeleton />
      ) : (
        <>
          <Card>
            <ReactECharts
              style={{ height: 260 }}
              option={{
                ...baseOption(dark),
                xAxis: { type: "category", data: rows.map((r) => r.name), axisLabel: { rotate: 30, color: c.ash } },
                yAxis: { type: "value", splitLine: { lineStyle: { color: c.stone } } },
                series: [{ type: "bar", data: rows.map((r) => r.success_count), itemStyle: { color: c.forest } }],
              }}
            />
          </Card>
          <Card className="mt-4 p-0">
            <Table>
              <THead>
                <TR>
                  <TH>账号</TH>
                  <TH>套餐</TH>
                  <TH>总用量</TH>
                  <TH>Auto</TH>
                  <TH>API</TH>
                  <TH>按需</TH>
                  <TH>成功</TH>
                  <TH>更新</TH>
                  <TH>错误</TH>
                </TR>
              </THead>
              <TBody>
                {rows.map((r) => (
                  <TR key={r.name || "unknown"}>
                    <TD>
                      <Link to={`/accounts/${encodeURIComponent(r.name || "")}`} className="text-ember hover:underline">
                        {r.name || "—"}
                      </Link>
                    </TD>
                    <TD className="font-mono text-[12px]">{r.vendor?.plan_label || r.vendor?.plan_badge || "—"}</TD>
                    <TD className="font-mono text-[12px]">
                      {r.vendor?.total_percent_used != null ? `${r.vendor.total_percent_used.toFixed(0)}%` : "—"}
                      {r.vendor?.plan_used_cents != null && r.vendor.plan_limit_cents != null
                        ? ` · ${formatUSDFromCents(r.vendor.plan_used_cents)} / ${formatUSDFromCents(r.vendor.plan_limit_cents)}`
                        : ""}
                    </TD>
                    <TD className="font-mono text-[12px]">{r.vendor?.auto_percent_used != null ? `${r.vendor.auto_percent_used.toFixed(0)}%` : "—"}</TD>
                    <TD className="font-mono text-[12px]">{r.vendor?.api_percent_used != null ? `${r.vendor.api_percent_used.toFixed(0)}%` : "—"}</TD>
                    <TD className="font-mono text-[12px]">{r.vendor?.on_demand_used_cents != null ? formatUSDFromCents(r.vendor.on_demand_used_cents) : "—"}</TD>
                    <TD className="font-mono text-[12px]">{formatNumber(r.success_count)}</TD>
                    <TD className="font-mono text-[12px]">{formatTime(r.usage_updated_at || r.updated_at)}</TD>
                    <TD className="text-crimson">{r.usage_error || r.error || "—"}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </Card>
        </>
      )}
    </div>
  );
}
