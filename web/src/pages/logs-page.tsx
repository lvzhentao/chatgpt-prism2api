import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { EmptyState } from "@/components/shared/empty-state";
import { FilterSelect } from "@/components/shared/filter-select";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { adminApi } from "@/lib/api";
import { formatTime, statusTone } from "@/lib/format";

const STATUS_OPTS = [
  { value: "200", label: "200" },
  { value: "401", label: "401" },
  { value: "403", label: "403" },
  { value: "404", label: "404" },
  { value: "429", label: "429" },
  { value: "502", label: "502" },
  { value: "503", label: "503" },
  { value: "4xx", label: "4xx" },
  { value: "5xx", label: "5xx" },
];

function keysOf(m?: Record<string, number>) {
  return Object.keys(m ?? {})
    .filter(Boolean)
    .sort()
    .map((k) => ({ value: k, label: k }));
}

export function LogsPage() {
  const [account, setAccount] = useState("");
  const [model, setModel] = useState("");
  const [status, setStatus] = useState("");
  const [path, setPath] = useState("");
  const [failClass, setFailClass] = useState("");
  const accounts = useQuery({ queryKey: ["accounts"], queryFn: adminApi.accounts });
  const models = useQuery({ queryKey: ["models"], queryFn: adminApi.models });
  const stats = useQuery({ queryKey: ["stats"], queryFn: adminApi.stats });
  const q = useQuery({
    queryKey: ["logs", account, model, status, path, failClass],
    queryFn: () => adminApi.logs({ account, model, status, path, fail_class: failClass, limit: 200 }),
  });

  const accountOpts = useMemo(() => {
    const names = new Set((accounts.data?.accounts ?? []).map((a) => a.name).filter(Boolean));
    for (const k of Object.keys((stats.data?.by_account as Record<string, number>) ?? {})) names.add(k);
    return [...names].sort().map((k) => ({ value: k, label: k }));
  }, [accounts.data, stats.data]);

  const modelOpts = useMemo(() => {
    const names = new Set((models.data?.models ?? []).map((m) => m.id).filter(Boolean));
    for (const k of Object.keys((stats.data?.by_model as Record<string, number>) ?? {})) names.add(k);
    return [...names].sort().map((k) => ({ value: k, label: k }));
  }, [models.data, stats.data]);

  const pathOpts = useMemo(() => keysOf(stats.data?.by_path as Record<string, number> | undefined), [stats.data]);
  const failOpts = useMemo(() => keysOf(stats.data?.by_fail_class as Record<string, number> | undefined), [stats.data]);

  return (
    <div>
      <PageHeader
        title="请求日志"
        description="下游调用的方法、路径、账号、头体与耗时。敏感字段已脱敏。"
        actions={
          <Button
            variant="secondary"
            onClick={async () => {
              await adminApi.clearLogs();
              toast.success("已清空");
              q.refetch();
            }}
          >
            清空
          </Button>
        }
      />
      <div className="mb-4 grid gap-2 sm:grid-cols-2 lg:grid-cols-5">
        <FilterSelect value={account} onChange={setAccount} options={accountOpts} placeholder="全部账号" />
        <FilterSelect value={model} onChange={setModel} options={modelOpts} placeholder="全部模型" />
        <FilterSelect value={status} onChange={setStatus} options={STATUS_OPTS} placeholder="全部状态" />
        <FilterSelect value={path} onChange={setPath} options={pathOpts} placeholder="全部路径" />
        <FilterSelect value={failClass} onChange={setFailClass} options={failOpts} placeholder="全部分类" />
      </div>
      {q.isLoading ? (
        <PageSkeleton kind="table" />
      ) : (q.data?.entries ?? []).length === 0 ? (
        <EmptyState title="还没有请求日志。对话打进来后会出现在这里。" />
      ) : (
        <Card className="p-0">
          <Table>
            <THead>
              <TR>
                <TH>时间</TH>
                <TH>状态</TH>
                <TH>路径</TH>
                <TH>账号</TH>
                <TH>模型</TH>
                <TH>分类</TH>
                <TH>延迟</TH>
              </TR>
            </THead>
            <TBody>
              {(q.data?.entries ?? []).map((e) => (
                <TR key={`${e.id}-${e.request_id}`}>
                  <TD className="font-mono text-[12px]">{formatTime(e.time)}</TD>
                  <TD>
                    <Badge tone={statusTone(e.status) === "ok" ? "ok" : statusTone(e.status) === "warn" ? "warn" : "err"}>
                      {e.status}
                    </Badge>
                  </TD>
                  <TD>
                    <Link to={`/logs/${e.id}`} className="font-mono text-[12px] text-ember hover:underline">
                      {e.method} {e.path}
                    </Link>
                  </TD>
                  <TD>{e.account || "—"}</TD>
                  <TD className="font-mono text-[12px]">{e.model || "—"}</TD>
                  <TD className="font-mono text-[12px]">{e.fail_class || "—"}</TD>
                  <TD className="font-mono text-[12px]">{e.latency_ms}ms</TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </Card>
      )}
    </div>
  );
}
