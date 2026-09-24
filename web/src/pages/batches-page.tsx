import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { EmptyState } from "@/components/shared/empty-state";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { adminApi } from "@/lib/api";
import { formatTime } from "@/lib/format";

export function BatchesPage() {
  const q = useQuery({ queryKey: ["batches"], queryFn: adminApi.batches, refetchInterval: 8000 });
  const rows = q.data?.batches ?? [];
  return (
    <div>
      <PageHeader title="消息批次" description="Anthropic Message Batches 落盘记录。" />
      {q.isLoading ? <PageSkeleton kind="table" /> : rows.length === 0 ? (
        <EmptyState title="还没有批次。客户端调用 /v1/messages/batches 后会出现在这里。" />
      ) : (
        <Card className="p-0">
          <Table>
            <THead>
              <TR>
                <TH>ID</TH>
                <TH>状态</TH>
                <TH>成功 / 失败</TH>
                <TH>创建</TH>
              </TR>
            </THead>
            <TBody>
              {rows.map((b) => (
                <TR key={b.id}>
                  <TD>
                    <Link to={`/batches/${encodeURIComponent(b.id)}`} className="font-mono text-[12px] text-ember hover:underline">
                      {b.id}
                    </Link>
                  </TD>
                  <TD>
                    <Badge>{b.processing_status}</Badge>
                  </TD>
                <TD className="font-mono text-[12px]">
                  {b.request_counts?.succeeded ?? 0} / {b.request_counts?.errored ?? 0}
                </TD>
                  <TD className="font-mono text-[12px]">{formatTime(b.created_at)}</TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </Card>
      )}
    </div>
  );
}

