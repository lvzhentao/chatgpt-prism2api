import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { PageHeader } from "@/components/shared/page-header";
import { EmptyState } from "@/components/shared/empty-state";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { RowActions } from "@/components/shared/row-actions";
import { adminApi } from "@/lib/api";

export function GroupsPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["groups"], queryFn: adminApi.groups });
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [desc, setDesc] = useState("");
  const create = useMutation({
    mutationFn: () => adminApi.createGroup({ name, description: desc }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["groups"] });
      setOpen(false);
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <div>
      <PageHeader title="分组" description="账号与密钥共用组名；组可覆盖 RPM / 日限。" actions={<Button onClick={() => setOpen(true)}>新建</Button>} />
      {q.isLoading ? <PageSkeleton kind="table" /> : (q.data?.groups ?? []).length === 0 ? (
        <EmptyState title="还没有分组。建一个组，再把账号和密钥绑上去。" action={<Button onClick={() => setOpen(true)}>新建</Button>} />
      ) : (
      <Card className="p-0">
        <Table>
          <THead>
            <TR>
              <TH>名称</TH>
              <TH>账号</TH>
              <TH>密钥</TH>
              <TH>RPM / 日限</TH>
              <TH>操作</TH>
            </TR>
          </THead>
          <TBody>
            {(q.data?.groups ?? []).map((g) => (
              <TR key={g.name}>
                <TD>
                  <Link to={`/groups/${encodeURIComponent(g.name)}`} className="text-ember hover:underline">
                    {g.name}
                  </Link>
                </TD>
                <TD>{g.credential_count}</TD>
                <TD>{g.client_key_count}</TD>
                <TD className="font-mono text-[12px]">
                  {g.rpm || 0} / {g.daily_max || 0}
                </TD>
                <TD>
                  <RowActions
                    to={`/groups/${encodeURIComponent(g.name)}`}
                    onDelete={async () => {
                      const extra =
                        g.credential_count || g.client_key_count
                          ? ` 绑定的 ${g.credential_count} 个账号、${g.client_key_count} 个密钥会解绑。`
                          : "";
                      if (!window.confirm(`删除分组 ${g.name}？${extra}`)) return;
                      try {
                        await adminApi.deleteGroup(g.name, true);
                        toast.success(`已删除 ${g.name}`);
                        void qc.invalidateQueries({ queryKey: ["groups"] });
                        void qc.invalidateQueries({ queryKey: ["keys"] });
                        void qc.invalidateQueries({ queryKey: ["accounts"] });
                      } catch (e) {
                        toast.error(e instanceof Error ? e.message : "删除失败");
                      }
                    }}
                  />
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      </Card>
      )}
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogTitle>新建分组</DialogTitle>
          <div className="mt-4 space-y-3">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label>备注</Label>
              <Input value={desc} onChange={(e) => setDesc(e.target.value)} />
            </div>
            <Button onClick={() => create.mutate()} disabled={!name}>
              创建
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}
