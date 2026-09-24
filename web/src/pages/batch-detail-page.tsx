import { useQuery } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { Card } from "@/components/ui/card";
import { JsonView } from "@/components/shared/json-view";
import { PageHeader } from "@/components/shared/page-header";
import { adminApi } from "@/lib/api";
import { formatTime } from "@/lib/format";

export function BatchDetailPage() {
  const { id = "" } = useParams();
  const q = useQuery({ queryKey: ["batch", id], queryFn: () => adminApi.batch(id), refetchInterval: 5000 });
  if (q.isError) return <p className="text-crimson">找不到批次。</p>;
  if (!q.data) return <p className="text-ash">加载批次…</p>;
  const b = q.data;
  return (
    <div>
      <PageHeader title={b.id} description={`${b.processing_status} · 创建于 ${formatTime(b.created_at)}`} />
      <Card>
        <JsonView value={b} />
      </Card>
    </div>
  );
}
