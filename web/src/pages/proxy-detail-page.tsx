import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { JsonView } from "@/components/shared/json-view";
import { PageHeader } from "@/components/shared/page-header";
import { adminApi } from "@/lib/api";

export function ProxyDetailPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const q = useQuery({ queryKey: ["proxy", id], queryFn: () => adminApi.proxy(id) });
  const [name, setName] = useState<string>();
  const [resinUrl, setResinUrl] = useState<string>();
  const [resinPlatform, setResinPlatform] = useState<string>();
  const [httpProxy, setHttpProxy] = useState<string>();
  if (q.isError) return <p className="text-crimson">找不到出口。</p>;
  if (!q.data) return <p className="text-ash">加载出口…</p>;
  const p = q.data;
  return (
    <div>
      <PageHeader
        title={p.name}
        description="账号绑定此出口后，往上游的请求都经它发出。"
        actions={
          <>
            <Button
              variant="secondary"
              onClick={async () => {
                const t = await adminApi.probeProxy(id);
                toast.success("探测任务已排队");
                nav(`/tasks/${t.task.id}`);
              }}
            >
              探测出口 IP
            </Button>
            <Button
              variant="destructive"
              onClick={async () => {
                if (!window.confirm(`删除出口 ${p.name}？未绑定账号会改走全局出口。`)) return;
                await adminApi.deleteProxy(id);
                toast.success("已删除");
                nav("/proxies");
              }}
            >
              删除
            </Button>
          </>
        }
      />
      <Card className="max-w-xl space-y-3">
        <div className="space-y-1.5">
          <Label>名称</Label>
          <Input value={name ?? p.name} onChange={(e) => setName(e.target.value)} />
        </div>
        {p.kind === "resin" ? (
          <>
            <div className="space-y-1.5">
              <Label>Resin URL</Label>
              <Input value={resinUrl ?? p.resin_url ?? ""} onChange={(e) => setResinUrl(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label>平台</Label>
              <Input value={resinPlatform ?? p.resin_platform ?? "Default"} onChange={(e) => setResinPlatform(e.target.value)} />
            </div>
          </>
        ) : null}
        {p.kind === "http" ? (
          <div className="space-y-1.5">
            <Label>HTTP 代理</Label>
            <Input value={httpProxy ?? p.http_proxy ?? ""} onChange={(e) => setHttpProxy(e.target.value)} />
          </div>
        ) : null}
        <label className="flex items-center justify-between">
          <span className="text-[13px] text-ash">启用</span>
          <Switch
            checked={p.enabled}
            onCheckedChange={async (enabled) => {
              await adminApi.patchProxy(id, { enabled });
              q.refetch();
            }}
          />
        </label>
        <label className="flex items-center justify-between">
          <span className="text-[13px] text-ash">默认</span>
          <Switch
            checked={p.is_default}
            onCheckedChange={async (is_default) => {
              await adminApi.patchProxy(id, { is_default });
              q.refetch();
            }}
          />
        </label>
        <Button
          onClick={async () => {
            await adminApi.patchProxy(id, {
              name: name ?? p.name,
              resin_url: resinUrl ?? p.resin_url,
              resin_platform: resinPlatform ?? p.resin_platform,
              http_proxy: httpProxy ?? p.http_proxy,
            });
            toast.success("已保存");
            q.refetch();
          }}
        >
          保存
        </Button>
      </Card>
      <Card className="mt-4">
        <JsonView value={p} />
      </Card>
    </div>
  );
}
