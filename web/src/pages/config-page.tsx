import { useMutation, useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { PageHeader } from "@/components/shared/page-header";
import { adminApi } from "@/lib/api";

const groups: Record<string, string[]> = {
  runtime: ["api_base_url", "website_url", "cache_ttl_seconds", "log_max_entries", "log_retention_days", "listen_addr", "mock_mode", "history_compress", "system_prompt", "system_prompt_mode", "identity_guard_enabled", "identity_answer_text"],
  routing: ["routing_strategy", "session_affinity", "session_affinity_ttl", "request_retry", "max_retry_credentials", "max_retry_interval", "disable_cooling"],
  concurrency: ["batch_concurrency", "account_concurrency", "account_concurrency_429"],
  proxy: ["proxy_url", "resin_enabled", "resin_url", "resin_platform"],
  security: ["admin_username", "admin_password", "api_key_auth", "allow_remote_admin"],
  models: ["model_routes"],
};

export function ConfigPage() {
  const schema = useQuery({ queryKey: ["config-schema"], queryFn: adminApi.configSchema });
  const cfg = useQuery({ queryKey: ["config"], queryFn: adminApi.config });
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const fields = schema.data?.fields ?? [];
  const merged = useMemo(() => ({ ...(cfg.data ?? {}), ...draft }), [cfg.data, draft]);

  const save = useMutation({
    mutationFn: () => adminApi.updateConfig(draft),
    onSuccess: (res) => {
      toast.success(res.restart_required ? "已保存，部分字段需重启" : "已保存并即时生效");
      setDraft({});
      void cfg.refetch();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const renderField = (name: string) => {
    const meta = fields.find((f) => f.name === name);
    if (!meta) return null;
    const value = merged[name];
    const title = meta.description || name;
    if (meta.type === "bool") {
      return (
        <label key={name} className="flex items-center justify-between gap-4 border-b border-border/60 py-3">
          <div>
            <div className="text-[14px] text-ink">{title}</div>
            <div className="font-mono text-[11px] text-ash">{name}</div>
          </div>
          <Switch checked={Boolean(value)} onCheckedChange={(v) => setDraft((d) => ({ ...d, [name]: v }))} />
        </label>
      );
    }
    if (meta.type === "map" || meta.type === "text") {
      return (
        <div key={name} className="space-y-1.5 py-3">
          <Label>{title}</Label>
          <p className="font-mono text-[11px] text-ash">{name}</p>
          <Textarea
            className="font-mono text-[12px]"
            value={typeof value === "string" ? value : JSON.stringify(value ?? {}, null, 2)}
            onChange={(e) => {
              if (meta.type === "map") {
                try {
                  setDraft((d) => ({ ...d, [name]: JSON.parse(e.target.value || "{}") }));
                } catch {
                  setDraft((d) => ({ ...d, [name]: e.target.value }));
                }
              } else {
                setDraft((d) => ({ ...d, [name]: e.target.value }));
              }
            }}
          />
        </div>
      );
    }
    return (
      <div key={name} className="space-y-1.5 py-3">
        <Label>
          {title}
          {meta.restart_required ? <span className="ml-2 font-mono text-[11px] text-amber">需重启</span> : null}
        </Label>
        <p className="font-mono text-[11px] text-ash">{name}</p>
        <Input
          type={meta.secret ? "password" : meta.type === "int" ? "number" : "text"}
          value={value == null ? "" : String(value)}
          onChange={(e) =>
            setDraft((d) => ({
              ...d,
              [name]: meta.type === "int" ? Number(e.target.value) : e.target.value,
            }))
          }
        />
      </div>
    );
  };

  return (
    <div>
      <PageHeader
        title="配置中心"
        description="热配置即时生效。批量 CRUD 并发在「并发」页，Resin 出口在「出口」。跨机域名请用 Resin 反代，不要配 CONNECT / SOCKS。"
        actions={
          <Button onClick={() => save.mutate()} disabled={Object.keys(draft).length === 0 || save.isPending}>
            保存更改
          </Button>
        }
      />
      <Tabs defaultValue="runtime">
        <TabsList>
          <TabsTrigger value="runtime">运行时</TabsTrigger>
          <TabsTrigger value="routing">调度</TabsTrigger>
          <TabsTrigger value="concurrency">并发</TabsTrigger>
          <TabsTrigger value="proxy">出口</TabsTrigger>
          <TabsTrigger value="models">模型路由</TabsTrigger>
          <TabsTrigger value="security">安全</TabsTrigger>
        </TabsList>
        {Object.entries(groups).map(([key, names]) => (
          <TabsContent key={key} value={key}>
            {key === "concurrency" ? (
              <p className="mb-3 max-w-2xl font-serif text-[16px] text-driftwood">
                批量启用 / 停用 / 删除 / 导入 / 刷新用量 / 探测出口都走同一把并发锁，避免把上游打满。
              </p>
            ) : null}
            {key === "proxy" ? (
              <p className="mb-3 max-w-2xl font-serif text-[16px] text-driftwood">
                账号未绑定代理池条目时，走这里的全局 Resin 或 HTTP 正代。精细到账号请用「代理池」。
              </p>
            ) : null}
            {key === "models" ? (
              <p className="mb-3 max-w-2xl font-serif text-[16px] text-driftwood">
                日常改路由请用「
                <Link to="/models" className="text-ember hover:underline">
                  模型目录
                </Link>
                」：按厂家分组、带 Logo，可一键恢复透传。这里只留原始 JSON。
              </p>
            ) : null}
            <Card className="max-w-2xl">{names.map(renderField)}</Card>
          </TabsContent>
        ))}
      </Tabs>
    </div>
  );
}
