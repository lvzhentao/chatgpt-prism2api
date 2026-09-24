import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { JsonView } from "@/components/shared/json-view";
import { PageHeader } from "@/components/shared/page-header";
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

function sseLabel(s: SSEStatus) {
  if (s === "live") return "实时推送";
  if (s === "retry") return "重连中";
  if (s === "connecting") return "连接中";
  return "已断开";
}

type LogLine = { time: string; level: string; message: string };

function applyEvent(prev: TaskItem | null, data: unknown): TaskItem | null {
  const payload = data as { task?: TaskItem; log?: LogLine } & Partial<TaskItem>;
  const next = payload.task ?? (payload.id ? (payload as TaskItem) : prev);
  if (!next) return prev;
  if (!payload.log) return next;
  const logs = next.logs ?? prev?.logs ?? [];
  const dup = logs.some((l) => l.time === payload.log!.time && l.message === payload.log!.message);
  if (dup) return { ...next, logs };
  return { ...next, logs: [...logs, payload.log] };
}

export function TaskDetailPage() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["task", id], queryFn: () => adminApi.task(id) });
  const [live, setLive] = useState<TaskItem | null>(null);
  const [sse, setSse] = useState<SSEStatus>("connecting");
  const bottom = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setLive(null);
    const stop = openSSE(
      `/api/admin/tasks/${id}/events`,
      (_ev, data) => {
        setLive((prev) => {
          const next = applyEvent(prev, data);
          if (next) qc.setQueryData(["task", id], next);
          return next;
        });
      },
      undefined,
      setSse,
    );
    return stop;
  }, [id, qc]);

  const t = live ?? q.data;
  useEffect(() => {
    bottom.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [t?.logs.length]);

  if (q.isError && !live) return <p className="text-crimson">找不到任务。</p>;
  if (!t) return <p className="text-ash">加载任务…</p>;
  return (
    <div>
      <PageHeader
        title={t.title}
        description={`${t.type} · ${t.message || t.status}`}
        actions={
          t.cancelable && (t.status === "running" || t.status === "pending") ? (
            <Button
              variant="secondary"
              onClick={async () => {
                await adminApi.cancelTask(id);
                toast.success("已请求取消");
              }}
            >
              取消
            </Button>
          ) : null
        }
      />
      <div className="mb-3 flex flex-wrap gap-3 font-mono text-[12px] text-ash">
        <Badge tone={tone(t.status)}>{t.status}</Badge>
        <Badge tone={sse === "live" ? "ok" : sse === "retry" ? "warn" : "muted"}>{sseLabel(sse)}</Badge>
        <span>
          {t.current}/{t.total || 0}
        </span>
        <span>{formatTime(t.created_at)}</span>
        {t.error ? <span className="text-crimson">{t.error}</span> : null}
      </div>
      <ProgressBar current={t.current} total={t.total} className="mb-4" />
      <Card>
        <div className="mb-2 font-mono text-[12px] text-ash">流式日志</div>
        <ScrollArea className="h-[360px] rounded-md border border-border bg-parchment p-3">
          <div className="space-y-1 font-mono text-[12px]">
            {(t.logs ?? []).map((l, i) => (
              <div key={`${l.time}-${i}`} className={l.level === "error" ? "text-crimson" : "text-ink"}>
                <span className="text-ash">{formatTime(l.time)} </span>
                {l.message}
              </div>
            ))}
            <div ref={bottom} />
          </div>
        </ScrollArea>
      </Card>
      {t.result != null ? (
        <Card className="mt-4">
          <div className="mb-2 font-mono text-[12px] text-ash">结果</div>
          <JsonView value={t.result} />
        </Card>
      ) : null}
      {t.meta ? (
        <Card className="mt-4">
          <div className="mb-2 font-mono text-[12px] text-ash">元数据</div>
          <JsonView value={t.meta} />
        </Card>
      ) : null}
    </div>
  );
}
