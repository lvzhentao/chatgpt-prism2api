import { useQuery } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { Card } from "@/components/ui/card";
import { JsonView } from "@/components/shared/json-view";
import { PageHeader } from "@/components/shared/page-header";
import { adminApi } from "@/lib/api";
import { formatTime } from "@/lib/format";

export function LogDetailPage() {
  const { id = "" } = useParams();
  const q = useQuery({ queryKey: ["log", id], queryFn: () => adminApi.log(id) });
  if (q.isError) return <p className="text-crimson">找不到这条日志。</p>;
  if (!q.data) return <p className="text-ash">加载日志…</p>;
  const e = q.data;
  return (
    <div>
      <PageHeader title={`${e.method} ${e.path}`} description={`${e.status} · ${formatTime(e.time)} · ${e.request_id || e.id}`} />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <div className="mb-2 font-mono text-[12px] text-ash">请求头</div>
          <JsonView value={e.headers ?? {}} />
        </Card>
        <Card>
          <div className="mb-2 font-mono text-[12px] text-ash">请求体</div>
          <JsonView value={e.request_body || "—"} />
        </Card>
      </div>
      <Card className="mt-4">
        <JsonView value={e} />
      </Card>
    </div>
  );
}
