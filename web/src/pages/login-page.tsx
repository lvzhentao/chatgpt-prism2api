import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ApiError, adminApi } from "@/lib/api";
import { brand } from "@/lib/brand";
import { useAuthStore } from "@/stores/auth";

export function LoginPage() {
  const nav = useNavigate();
  const setSession = useAuthStore((s) => s.setSession);
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    adminApi
      .me()
      .then((r) => {
        if (!r.username) return;
        setSession(r.username, Boolean(r.must_change_password));
        nav(r.must_change_password ? "/password" : "/", { replace: true });
      })
      .catch(() => undefined);
  }, [nav, setSession]);

  return (
    <div className="flex min-h-dvh items-center justify-center px-4">
      <form
        className="w-full max-w-[360px]"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          try {
            const res = await adminApi.login(username, password);
            setSession(res.username, Boolean(res.must_change_password));
            nav(res.must_change_password ? "/password" : "/");
          } catch (err) {
            toast.error(err instanceof ApiError ? err.message : "登录失败");
          } finally {
            setBusy(false);
          }
        }}
      >
        <div
          className="grid h-10 w-10 place-items-center rounded-md text-[13px] font-semibold text-white"
          style={{ background: brand.primaryColor }}
        >
          {brand.logoText}
        </div>
        <p className="mt-4 font-mono text-[12px] text-ash">{brand.appName}</p>
        <h1 className="mt-2 text-[36px] leading-[1.2] tracking-[-0.72px] text-ink">管理后台</h1>
        <p className="mt-2 font-serif text-[16px] text-driftwood">用管理员账号进入账号池与任务中心。</p>
        <div className="mt-8 space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="u">用户名</Label>
            <Input id="u" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="p">密码</Label>
            <Input
              id="p"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </div>
          <Button type="submit" className="w-full" disabled={busy}>
            {busy ? "登录中…" : "登录"}
          </Button>
        </div>
      </form>
    </div>
  );
}
