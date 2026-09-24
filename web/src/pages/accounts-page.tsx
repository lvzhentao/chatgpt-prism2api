import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { LayoutGrid, List, Plus, Upload } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDesc, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { AccountCard } from "@/components/accounts/account-card";
import { EmptyState } from "@/components/shared/empty-state";
import { PageHeader } from "@/components/shared/page-header";
import { PageSkeleton, Stagger } from "@/components/shared/page-skeleton";
import { StatusDot } from "@/components/shared/status-dot";
import { adminApi } from "@/lib/api";
import { cn } from "@/lib/utils";
import { formatDisableReason, formatTime } from "@/lib/format";
import { useUIStore } from "@/stores/ui";
import type { Account } from "@/types/api";

async function submitImport(raw: string, overwrite: boolean) {
  const text = raw.trim();
  if (!text) throw new Error("没有可导入的内容");
  if (text.startsWith("{") || text.startsWith("[")) {
    const parsed = JSON.parse(text) as { accounts?: unknown[]; text?: string } | unknown[];
    if (Array.isArray(parsed)) {
      return adminApi.importAccounts({ accounts: parsed, overwrite });
    }
    if (parsed.text && !parsed.accounts?.length) {
      return adminApi.importAccounts({ text: parsed.text, overwrite });
    }
    return adminApi.importAccounts({ accounts: parsed.accounts ?? [], text: parsed.text, overwrite });
  }
  return adminApi.importAccounts({ text, overwrite });
}

function toneOf(a: Account): "ok" | "warn" | "err" | "muted" {
  if (!a.enabled) return "muted";
  if (a.cooldown_until && a.cooldown_until * 1000 > Date.now()) return "warn";
  if (a.logged_in) return "ok";
  return "err";
}

export function AccountsPage() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const view = useUIStore((s) => s.accountView);
  const setView = useUIStore((s) => s.setAccountView);
  const q = useQuery({ queryKey: ["accounts"], queryFn: adminApi.accounts, refetchInterval: 8000 });
  const proxies = useQuery({ queryKey: ["proxies"], queryFn: adminApi.proxies });
  const fileRef = useRef<HTMLInputElement>(null);
  const [open, setOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [proxyOpen, setProxyOpen] = useState(false);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [accessToken, setAccessToken] = useState("");
  const [importText, setImportText] = useState("");
  const [picked, setPicked] = useState<string[]>([]);
  const [loginUrl, setLoginUrl] = useState("");
  const [loginName, setLoginName] = useState("");
  const [proxyId, setProxyId] = useState("");
  const [overwrite, setOverwrite] = useState(false);

  useEffect(() => {
    if (!loginName) return;
    const t = window.setInterval(async () => {
      const list = await adminApi.accounts();
      const a = list.accounts.find((x) => x.name === loginName);
      if (a?.logged_in) {
        toast.success(`${loginName} 已完成登录`);
        setLoginName("");
        void qc.invalidateQueries({ queryKey: ["accounts"] });
      }
    }, 2500);
    return () => window.clearInterval(t);
  }, [loginName, qc]);

  const create = useMutation({
    mutationFn: () =>
      adminApi.createAccount({
        name: name || undefined,
        email: email || undefined,
        api_key: apiKey || undefined,
        access_token: accessToken || undefined,
      }),
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: ["accounts"] });
      if (res.login_url) {
        setLoginUrl(res.login_url);
        setLoginName(res.name);
        toast.success("已生成登录链接，完成浏览器授权后会自动入池");
      } else {
        toast.success("账号已写入");
        setOpen(false);
      }
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const batch = useMutation({
    mutationFn: ({ action, extra }: { action: string; extra?: Record<string, unknown> }) =>
      adminApi.accountActions(action, picked, extra),
    onSuccess: (res) => {
      toast.success("已提交任务");
      setPicked([]);
      void qc.invalidateQueries({ queryKey: ["tasks"] });
      nav(`/tasks/${res.task.id}`);
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const accounts = q.data?.accounts ?? [];
  const allChecked = useMemo(() => accounts.length > 0 && picked.length === accounts.length, [accounts, picked]);

  if (q.isLoading) {
    return (
      <div>
        <PageHeader title="账号" description="卡片或列表查看池内账号。" />
        <PageSkeleton kind="cards" />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="账号"
        description="卡片或列表查看池内账号。批量操作走任务中心，并受配置中心的全局并发限制。"
        actions={
          <>
            <Button variant={view === "card" ? "secondary" : "outline"} size="icon" aria-label="卡片" aria-pressed={view === "card"} onClick={() => setView("card")}>
              <LayoutGrid />
            </Button>
            <Button variant={view === "list" ? "secondary" : "outline"} size="icon" aria-label="列表" aria-pressed={view === "list"} onClick={() => setView("list")}>
              <List />
            </Button>
            <Button
              variant="outline"
              onClick={async () => {
                const data = await adminApi.exportAccounts();
                const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
                const a = document.createElement("a");
                a.href = URL.createObjectURL(blob);
                a.download = "accounts-export.json";
                a.click();
              }}
            >
              导出
            </Button>
            <Button variant="outline" onClick={() => setImportOpen(true)}>
              <Upload /> 导入
            </Button>
            <input
              ref={fileRef}
              type="file"
              accept="application/json,.json,.txt,text/plain"
              className="hidden"
              onChange={async (e) => {
                const file = e.target.files?.[0];
                e.target.value = "";
                if (!file) return;
                try {
                  const res = await submitImport(await file.text(), overwrite);
                  toast.success("导入任务已排队");
                  nav(`/tasks/${res.task.id}`);
                } catch (err) {
                  toast.error(err instanceof Error ? err.message : "导入失败");
                }
              }}
            />
            <Button onClick={() => setOpen(true)}>
              <Plus /> 添加
            </Button>
          </>
        }
      />
      <label className="mb-4 flex items-center gap-2 text-[13px] text-ash">
        <Switch checked={overwrite} onCheckedChange={setOverwrite} />
        导入时覆盖已有账号
      </label>
      {picked.length > 0 && (
        <div className="mb-4 flex flex-wrap gap-2">
          <Button size="sm" variant="secondary" onClick={() => batch.mutate({ action: "enable" })}>
            启用 {picked.length}
          </Button>
          <Button size="sm" variant="secondary" onClick={() => batch.mutate({ action: "disable" })}>
            停用
          </Button>
          <Button size="sm" variant="secondary" onClick={() => batch.mutate({ action: "refresh_usage" })}>
            刷新用量
          </Button>
          <Button size="sm" variant="secondary" onClick={() => batch.mutate({ action: "cooldown_clear" })}>
            清冷却
          </Button>
          <Button size="sm" variant="secondary" onClick={() => setProxyOpen(true)}>
            分配出口
          </Button>
          <Button size="sm" variant="destructive" onClick={() => batch.mutate({ action: "delete" })}>
            删除
          </Button>
        </div>
      )}
      {accounts.length === 0 ? (
        <EmptyState title="还没有账号。先用 API Key 或浏览器登录加一个。" action={<Button onClick={() => setOpen(true)}>添加账号</Button>} />
      ) : view === "card" ? (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 lg:grid-cols-3">
          {accounts.map((a, i) => (
            <Stagger key={a.name} i={Math.min(i, 11)} className="min-w-0">
            <AccountCard
              account={a}
              checked={picked.includes(a.name)}
              onCheck={(v) => setPicked((p) => (v ? [...p, a.name] : p.filter((x) => x !== a.name)))}
              onToggle={async (enabled) => {
                try {
                  await adminApi.patchAccount({ name: a.name, enabled });
                  void qc.invalidateQueries({ queryKey: ["accounts"] });
                } catch (e) {
                  toast.error(e instanceof Error ? e.message : "无法改状态");
                }
              }}
              onRefresh={async () => {
                try {
                  const res = await adminApi.refreshAccountUsage(a.name);
                  if (res.usage_error) toast.error(res.usage_error);
                  else toast.success("用量已刷新");
                  void qc.invalidateQueries({ queryKey: ["accounts"] });
                  void qc.invalidateQueries({ queryKey: ["usage"] });
                } catch (e) {
                  toast.error(e instanceof Error ? e.message : "刷新失败");
                }
              }}
              onDelete={async () => {
                if (!window.confirm(`删除账号 ${a.name}？`)) return;
                try {
                  await adminApi.deleteAccount(a.name);
                  toast.success(`已删除 ${a.name}`);
                  void qc.invalidateQueries({ queryKey: ["accounts"] });
                } catch (e) {
                  toast.error(e instanceof Error ? e.message : "删除失败");
                }
              }}
            />
            </Stagger>
          ))}
        </div>
      ) : (
        <Card className="p-0">
          <Table>
            <THead>
              <TR>
                <TH>
                  <Checkbox checked={allChecked} onCheckedChange={(v) => setPicked(v ? accounts.map((a) => a.name) : [])} />
                </TH>
                <TH>名称</TH>
                <TH>套餐</TH>
                <TH>状态</TH>
                <TH>分组</TH>
                <TH>优先/权重</TH>
                <TH>并发</TH>
                <TH>过期</TH>
              </TR>
            </THead>
            <TBody>
              {accounts.map((a) => (
                <TR key={a.name}>
                  <TD>
                    <Checkbox
                      checked={picked.includes(a.name)}
                      onCheckedChange={(v) => setPicked((p) => (v ? [...p, a.name] : p.filter((x) => x !== a.name)))}
                    />
                  </TD>
                  <TD>
                    <Link to={`/accounts/${encodeURIComponent(a.name)}`} className="text-ember hover:underline">
                      {a.email || a.name}
                    </Link>
                    {a.email && a.email !== a.name ? <div className="font-mono text-[11px] text-ash">{a.name}</div> : null}
                  </TD>
                  <TD className="font-mono text-[12px]">{a.plan_label || a.plan_badge || "—"}</TD>
                  <TD>
                    <span className="inline-flex items-center gap-2">
                      <StatusDot tone={toneOf(a)} />
                      {a.enabled ? "启用" : a.disable_reason ? formatDisableReason(a.disable_reason) : "停用"}
                    </span>
                  </TD>
                  <TD className="font-mono text-[12px]">{a.groups?.join(", ") || "—"}</TD>
                  <TD className="font-mono text-[12px]">
                    {a.priority ?? 0} / {a.weight ?? 1}
                  </TD>
                  <TD className="font-mono text-[12px]">
                    {a.inflight ?? 0}/{a.concurrency_limit && a.concurrency_limit > 0 ? a.concurrency_limit : "∞"}
                    {a.concurrency_degraded ? <span className="ml-1.5 text-ember" title="429 后并发已降级">429↓</span> : null}
                  </TD>
                  <TD className="font-mono text-[12px]">{formatTime(a.expires_at)}</TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </Card>
      )}

      <Dialog open={importOpen} onOpenChange={setImportOpen}>
        <DialogContent className="w-[min(720px,calc(100vw-24px))]">
          <DialogTitle>导入账号</DialogTitle>
          <DialogDesc>
            支持逐行 `email----password----token`、JSON 数组，或选择文件。站点特有字段由适配器 `ExchangeCredential` 消化。
          </DialogDesc>
          <div className="mt-4 space-y-3">
            <Textarea
              className="min-h-40 font-mono text-[12px]"
              value={importText}
              onChange={(e) => setImportText(e.target.value)}
              placeholder={"email----password----token\n{\"email\":\"name@example.com\",\"api_key\":\"sk-...\"}"}
            />
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" onClick={() => fileRef.current?.click()}>
                选择文件
              </Button>
              <Button
                disabled={!importText.trim()}
                onClick={async () => {
                  try {
                    const res = await submitImport(importText, overwrite);
                    toast.success("导入任务已排队");
                    setImportOpen(false);
                    setImportText("");
                    nav(`/tasks/${res.task.id}`);
                  } catch (err) {
                    toast.error(err instanceof Error ? err.message : "导入失败");
                  }
                }}
              >
                导入
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogTitle>添加账号</DialogTitle>
          <DialogDesc>填 Access Token 或 API Key 立刻入池；都留空则打开浏览器授权（需适配器实现 LoginURL）。</DialogDesc>
          <div className="mt-4 space-y-3">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="可空，默认用邮箱前缀" />
            </div>
            <div className="space-y-1.5">
              <Label>邮箱（可空）</Label>
              <Input value={email} onChange={(e) => setEmail(e.target.value)} placeholder="name@example.com" />
            </div>
            <div className="space-y-1.5">
              <Label>Session / Access Token（可空）</Label>
              <Textarea
                className="min-h-20 font-mono text-[12px]"
                value={accessToken}
                onChange={(e) => setAccessToken(e.target.value)}
                placeholder="access token / session token"
              />
            </div>
            <div className="space-y-1.5">
              <Label>API Key（可空）</Label>
              <Input value={apiKey} onChange={(e) => setApiKey(e.target.value)} />
            </div>
            {loginUrl ? (
              <a className={cn("block break-all text-[13px] text-ember underline")} href={loginUrl} target="_blank" rel="noreferrer">
                {loginUrl}
              </a>
            ) : null}
            {loginName ? <p className="font-mono text-[12px] text-ash">正在等待 {loginName} 完成授权…</p> : null}
            <Button onClick={() => create.mutate()} disabled={create.isPending}>
              {create.isPending ? "提交中…" : "创建"}
            </Button>
          </div>
        </DialogContent>
      </Dialog>

      <Dialog open={proxyOpen} onOpenChange={setProxyOpen}>
        <DialogContent>
          <DialogTitle>分配出口</DialogTitle>
          <DialogDesc>选中的账号将绑定该代理池条目，后续请求都从这条出口发出。</DialogDesc>
          <select className="mt-4 h-9 w-full rounded-md border border-border bg-bone px-3 text-[14px]" value={proxyId} onChange={(e) => setProxyId(e.target.value)}>
            <option value="">清除绑定（走默认）</option>
            {(proxies.data?.proxies ?? []).map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} ({p.kind})
              </option>
            ))}
          </select>
          <Button
            className="mt-4"
            onClick={() => {
              batch.mutate({ action: "assign_proxy", extra: { proxy_id: proxyId } });
              setProxyOpen(false);
            }}
          >
            提交任务
          </Button>
        </DialogContent>
      </Dialog>
    </div>
  );
}
