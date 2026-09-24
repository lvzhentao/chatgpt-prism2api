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
import { EmptyState } from "@/components/shared/empty-state";
import { GroupSelect } from "@/components/shared/group-select";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { confirmDelete, RowActions } from "@/components/shared/row-actions";
import { adminApi } from "@/lib/api";
import { formatNumber } from "@/lib/format";

export function KeysPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["keys"], queryFn: adminApi.keys });
  const groups = useQuery({ queryKey: ["groups"], queryFn: adminApi.groups });
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [group, setGroup] = useState("");
  const [plain, setPlain] = useState("");
  const create = useMutation({
    mutationFn: () => adminApi.createKey({ name, group: group || undefined }),
    onSuccess: (res) => {
      setPlain(res.key);
      void qc.invalidateQueries({ queryKey: ["keys"] });
      toast.success("密钥只显示这一次，请立刻复制");
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <div>
      <PageHeader
        title="入口密钥"
        description="客户端调用 /v1 时使用的 csk_*。创建时明文只返回一次。分组从已有组里选。"
        actions={<Button onClick={() => setOpen(true)}>新建</Button>}
      />
      {q.isLoading ? <PageSkeleton kind="table" /> : (q.data?.keys ?? []).length === 0 ? (
        <EmptyState title="还没有入口密钥。创建后把明文交给调用方，这里只留掩码。" action={<Button onClick={() => setOpen(true)}>新建</Button>} />
      ) : (
        <Card className="p-0">
          <Table>
            <THead>
              <TR>
                <TH>名称</TH>
                <TH>密钥</TH>
                <TH>分组</TH>
                <TH>调用</TH>
                <TH>状态</TH>
                <TH>操作</TH>
              </TR>
            </THead>
            <TBody>
              {(q.data?.keys ?? []).map((k) => (
                <TR key={k.id}>
                  <TD>
                    <Link to={`/keys/${k.id}`} className="text-ember hover:underline">
                      {k.name}
                    </Link>
                  </TD>
                  <TD className="font-mono text-[12px]">{k.masked_key}</TD>
                  <TD>
                    <GroupSelect
                      className="h-8 max-w-[220px]"
                      value={k.group}
                      groups={groups.data?.groups ?? []}
                      onChange={async (g) => {
                        try {
                          await adminApi.updateKey(k.id, { group: g });
                          void qc.invalidateQueries({ queryKey: ["keys"] });
                        } catch (e) {
                          toast.error(e instanceof Error ? e.message : "无法改分组");
                        }
                      }}
                    />
                  </TD>
                  <TD className="font-mono text-[12px]">{formatNumber(k.total_calls)}</TD>
                  <TD>{k.disabled ? "停用" : k.is_system ? "系统" : "启用"}</TD>
                  <TD>
                    <RowActions
                      to={`/keys/${k.id}`}
                      deleteDisabled={k.is_system}
                      deleteHint={k.is_system ? "系统密钥不能删除" : undefined}
                      onDelete={async () => {
                        if (k.is_system) return;
                        if (!confirmDelete(`密钥 ${k.name}`)) return;
                        try {
                          await adminApi.deleteKey(k.id);
                          toast.success(`已删除 ${k.name}`);
                          void qc.invalidateQueries({ queryKey: ["keys"] });
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
          <DialogTitle>新建入口密钥</DialogTitle>
          <div className="mt-4 space-y-3">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label>绑定分组</Label>
              <GroupSelect value={group} groups={groups.data?.groups ?? []} onChange={setGroup} />
              {(groups.data?.groups ?? []).length === 0 ? (
                <p className="text-[12px] text-ash">
                  还没有分组。先到「分组」建一个，再回来绑。
                </p>
              ) : null}
            </div>
            {plain ? <p className="break-all font-mono text-[12px] text-ember">{plain}</p> : null}
            <Button onClick={() => create.mutate()} disabled={!name || create.isPending}>
              创建
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}
