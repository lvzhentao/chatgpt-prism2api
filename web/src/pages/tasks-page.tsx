import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { EmptyState } from "@/components/shared/empty-state";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { ProgressBar } from "@/components/shared/progress-bar";
import { ScrollArea } from "@/components/ui/scroll-area";
import { adminApi } from "@/lib/api";
import { openSSE, type SSEStatus } from "@/lib/sse";
import { formatTime } from "@/lib/format";
import type { TaskItem } from "@/types/api";

function tone(s: string) {
  if (s === "success") return "ok" as const;
  if (s === "failed") return "err" as const;
  if (s === "running" || s === "pending") return "warn" as const;
  return "muted" as const;
}

type FeedLine = { id: string; title: string; time: string; level: string; message: string };

function sseLabel(s: SSEStatus) {
  if (s === "live") return "实时";
  if (s === "retry") return "重连中";
  if (s === "connecting") return "连接中";
  return "已断开";
}

export function TasksPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["tasks"], queryFn: adminApi.tasks, refetchInterval: 8000 });
  const [feed, setFeed] = useState<FeedLine[]>([]);
  const [sse, setSse] = useState<SSEStatus>("connecting");
  const rows = q.data?.tasks ?? [];

  useEffect(() => {
    const stop = openSSE(
      "/api/admin/tasks/events",
      (ev, data) => {
        const payload = data as { task?: TaskItem; log?: { time: string; level: string; message: string } };
        if (payload.task) {
          qc.setQueryData(["tasks"], (old: { tasks: TaskItem[]; total: number; concurrency: number } | undefined) => {
            if (!old) return old;
            const next = old.tasks.some((t) => t.id === payload.task!.id)
              ? old.tasks.map((t) => (t.id === payload.task!.id ? { ...t, ...payload.task } : t))
              : [payload.task!, ...old.tasks];
            return { ...old, tasks: next, total: next.length };
          });
        }
        if (ev === "log" && payload.log && payload.task) {
          setFeed((prev) =>
            [
              {
                id: payload.task!.id,
                title: payload.task!.title,
                time: payload.log!.time,
                level: payload.log!.level,
                message: payload.log!.message,
              },
              ...prev,
            ].slice(0, 80),
          );
        }
      },
      undefined,
      setSse,
    );
    return stop;
  }, [qc]);

  return (
    <div>
      <PageHeader
        title="任务中心"
        description={`长任务可回溯，日志经 SSE 实时推送。当前并发 ${q.data?.concurrency ?? "—"}，在配置中心「并发」页调整。`}
      />
      <div className="mb-4 font-mono text-[12px] text-ash">SSE {sseLabel(sse)}</div>
      {q.isLoading ? (
        <PageSkeleton kind="table" />
      ) : rows.length === 0 ? (
        <EmptyState title="还没有后台任务。批量启用、导入、刷新用量都会出现在这里。" />
      ) : (
        <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
          <Card className="p-0">
            <Table>
              <THead>
                <TR>
                  <TH>任务</TH>
                  <TH>状态</TH>
                  <TH>进度</TH>
                  <TH>时间</TH>
                </TR>
              </THead>
              <TBody>
                {rows.map((t) => (
                  <TR key={t.id}>
                    <TD>
                      <Link to={`/tasks/${t.id}`} className="text-ember hover:underline">
                        {t.title}
                      </Link>
                      <div className="font-mono text-[11px] text-ash">{t.type}</div>
                    </TD>
                    <TD>
                      <Badge tone={tone(t.status)}>{t.status}</Badge>
                    </TD>
                    <TD className="min-w-[140px]">
                      <ProgressBar current={t.current} total={t.total} />
                      <div className="mt-1 font-mono text-[12px] text-ash">
                        {t.current}/{t.total || "—"}
                      </div>
                    </TD>
                    <TD className="font-mono text-[12px]">{formatTime(t.created_at)}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </Card>
          <Card>
            <div className="mb-2 font-mono text-[12px] text-ash">实时日志</div>
            {feed.length === 0 ? (
              <p className="text-[13px] text-ash">运行中的任务日志会流到这里。</p>
            ) : (
              <ScrollArea className="h-[420px] rounded-md border border-border bg-parchment p-3">
                <div className="space-y-2 font-mono text-[12px]">
                  {feed.map((l, i) => (
                    <div key={`${l.id}-${l.time}-${i}`} className={l.level === "error" ? "text-crimson" : "text-ink"}>
                      <Link to={`/tasks/${l.id}`} className="text-ember hover:underline">
                        {l.title}
                      </Link>{" "}
                      <span className="text-ash">{formatTime(l.time)}</span>
                      <div>{l.message}</div>
                    </div>
                  ))}
                </div>
              </ScrollArea>
            )}
          </Card>
        </div>
      )}
    </div>
  );
}
