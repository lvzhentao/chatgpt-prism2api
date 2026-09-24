import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { GroupSelect } from "@/components/shared/group-select";
import { PageHeader } from "@/components/shared/page-header";
import { JsonView } from "@/components/shared/json-view";
import { adminApi } from "@/lib/api";
import { useState } from "react";

export function KeyDetailPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["key", id], queryFn: () => adminApi.key(id) });
  const groups = useQuery({ queryKey: ["groups"], queryFn: adminApi.groups });
  const [name, setName] = useState<string>();
  const [group, setGroup] = useState<string>();
  if (q.isError) return <p className="text-crimson">找不到密钥。</p>;
  if (!q.data) return <p className="text-ash">加载密钥…</p>;
  const k = q.data;
  return (
    <div>
      <PageHeader
        title={k.name}
        actions={
          <Button
            variant="destructive"
            onClick={async () => {
              if (!window.confirm(`删除密钥 ${k.name}？`)) return;
              await adminApi.deleteKey(id);
              nav("/keys");
            }}
          >
            删除
          </Button>
        }
      />
      <Card className="max-w-xl space-y-3">
        <div className="space-y-1.5">
          <Label>名称</Label>
          <Input value={name ?? k.name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="space-y-1.5">
          <Label>分组</Label>
          <GroupSelect value={group ?? k.group ?? ""} groups={groups.data?.groups ?? []} onChange={setGroup} />
        </div>
        <div className="flex items-center justify-between">
          <Label>停用</Label>
          <Switch
            checked={k.disabled}
            onCheckedChange={async (disabled) => {
              await adminApi.updateKey(id, { disabled });
              qc.invalidateQueries({ queryKey: ["key", id] });
            }}
          />
        </div>
        <p className="font-mono text-[12px] text-ash">{k.masked_key}</p>
        <Button
          onClick={async () => {
            await adminApi.updateKey(id, { name: name ?? k.name, group: group ?? k.group });
            toast.success("已保存");
            q.refetch();
          }}
        >
          保存
        </Button>
      </Card>
      <Card className="mt-4">
        <JsonView value={k} />
      </Card>
    </div>
  );
}
