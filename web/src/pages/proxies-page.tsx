import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { PageHeader } from "@/components/shared/page-header";
import { EmptyState } from "@/components/shared/empty-state";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { confirmDelete, RowActions } from "@/components/shared/row-actions";
import { adminApi } from "@/lib/api";

export function ProxiesPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["proxies"], queryFn: adminApi.proxies });
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [kind, setKind] = useState("resin");
  const [resinUrl, setResinUrl] = useState("");
  const [resinPlatform, setResinPlatform] = useState("Default");
  const [httpProxy, setHttpProxy] = useState("");
  const [isDefault, setDefault] = useState(false);

  const create = useMutation({
    mutationFn: () =>
      adminApi.createProxy({
        name,
        kind,
        resin_url: resinUrl,
        http_proxy: httpProxy,
        is_default: isDefault,
        resin_platform: resinPlatform,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["proxies"] });
      setOpen(false);
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <div>
      <PageHeader
        title="代理池"
        description="往上游发请求的出口。跨机域名只能走 Resin 反代，不要配 CONNECT / SOCKS。"
        actions={<Button onClick={() => setOpen(true)}>添加出口</Button>}
      />
      {q.isLoading ? <PageSkeleton kind="table" /> : (q.data?.proxies ?? []).length === 0 ? (
        <EmptyState title="还没有出口。加一条 Resin 反代，再绑到账号上。" action={<Button onClick={() => setOpen(true)}>添加出口</Button>} />
      ) : (
      <Card className="p-0">
        <Table>
          <THead>
            <TR>
              <TH>名称</TH>
              <TH>类型</TH>
              <TH>默认</TH>
              <TH>最近出口 IP</TH>
              <TH>操作</TH>
            </TR>
          </THead>
          <TBody>
            {(q.data?.proxies ?? []).map((p) => (
              <TR key={p.id}>
                <TD>
                  <Link to={`/proxies/${p.id}`} className="text-ember hover:underline">
                    {p.name}
                  </Link>
                </TD>
                <TD className="font-mono text-[12px]">{p.kind}</TD>
                <TD>{p.is_default ? <Badge tone="ok">默认</Badge> : "—"}</TD>
                <TD className="font-mono text-[12px]">{p.last_probe_ip || p.last_error || "—"}</TD>
                <TD>
                  <RowActions
                    to={`/proxies/${p.id}`}
                    onDelete={async () => {
                      if (!confirmDelete(`出口 ${p.name}`)) return;
                      try {
                        await adminApi.deleteProxy(p.id);
                        toast.success(`已删除 ${p.name}`);
                        void qc.invalidateQueries({ queryKey: ["proxies"] });
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
          <DialogTitle>添加出口</DialogTitle>
          <div className="mt-4 space-y-3">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label>类型</Label>
              <select
                className="h-9 w-full rounded-md border border-border bg-bone px-3 text-[14px]"
                value={kind}
                onChange={(e) => setKind(e.target.value)}
              >
                <option value="resin">Resin 反代</option>
                <option value="http">HTTP 正代</option>
                <option value="direct">直连</option>
              </select>
            </div>
            {kind === "resin" ? (
              <>
                <div className="space-y-1.5">
                  <Label>Resin URL</Label>
                  <Input value={resinUrl} onChange={(e) => setResinUrl(e.target.value)} placeholder="https://resin.example.com/<token>" />
                </div>
                <div className="space-y-1.5">
                  <Label>平台</Label>
                  <Input value={resinPlatform} onChange={(e) => setResinPlatform(e.target.value)} placeholder="Default / NoHK" />
                </div>
              </>
            ) : null}
            {kind === "http" ? (
              <div className="space-y-1.5">
                <Label>HTTP 代理</Label>
                <Input value={httpProxy} onChange={(e) => setHttpProxy(e.target.value)} placeholder="http://127.0.0.1:7890" />
              </div>
            ) : null}
            <label className="flex items-center justify-between">
              <span className="text-[13px] text-ash">设为默认出口</span>
              <Switch checked={isDefault} onCheckedChange={setDefault} />
            </label>
            <Button onClick={() => create.mutate()} disabled={!name}>
              创建
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}
