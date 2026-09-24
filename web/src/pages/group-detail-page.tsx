import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { PageHeader } from "@/components/shared/page-header";
import { adminApi } from "@/lib/api";

export function GroupDetailPage() {
  const { name = "" } = useParams();
  const nav = useNavigate();
  const q = useQuery({ queryKey: ["group", name], queryFn: () => adminApi.group(name) });
  const [desc, setDesc] = useState<string>();
  const [rpm, setRpm] = useState<string>();
  const [daily, setDaily] = useState<string>();
  if (q.isError) return <p className="text-crimson">找不到分组。</p>;
  if (!q.data) return <p className="text-ash">加载分组…</p>;
  const g = q.data;
  return (
    <div>
      <PageHeader
        title={g.name}
        actions={
          <Button
            variant="destructive"
            onClick={async () => {
              if (!window.confirm(`删除分组 ${g.name}？绑定的账号和密钥会解绑。`)) return;
              await adminApi.deleteGroup(name, true);
              nav("/groups");
            }}
          >
            强制删除
          </Button>
        }
      />
      <Card className="max-w-xl space-y-3">
        <p className="font-mono text-[12px] text-ash">
          账号 {g.credential_count} · 密钥 {g.client_key_count}
        </p>
        <div className="space-y-1.5">
          <Label>备注</Label>
          <Input value={desc ?? g.description ?? ""} onChange={(e) => setDesc(e.target.value)} />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1.5">
            <Label>RPM</Label>
            <Input value={rpm ?? String(g.rpm ?? 0)} onChange={(e) => setRpm(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label>日限</Label>
            <Input value={daily ?? String(g.daily_max ?? 0)} onChange={(e) => setDaily(e.target.value)} />
          </div>
        </div>
        <Button
          onClick={async () => {
            await adminApi.updateGroup(name, {
              description: desc ?? g.description,
              rpm: Number(rpm ?? g.rpm ?? 0),
              daily_max: Number(daily ?? g.daily_max ?? 0),
            });
            toast.success("已保存");
            q.refetch();
          }}
        >
          保存
        </Button>
      </Card>
    </div>
  );
}
