import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ApiError, adminApi } from "@/lib/api";
import { useAuthStore } from "@/stores/auth";

export function PasswordPage() {
  const nav = useNavigate();
  const setSession = useAuthStore((s) => s.setSession);
  const username = useAuthStore((s) => s.username);
  const [pw, setPw] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <div className="flex min-h-dvh items-center justify-center px-4">
      <form
        className="w-full max-w-[360px]"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          try {
            await adminApi.password(pw);
            setSession(username, false);
            toast.success("密码已更新");
            nav("/");
          } catch (err) {
            toast.error(err instanceof ApiError ? err.message : "改密失败");
          } finally {
            setBusy(false);
          }
        }}
      >
        <h1 className="text-[26px] tracking-[-0.312px] text-ink">更换默认密码</h1>
        <p className="mt-2 font-serif text-[16px] text-driftwood">当前仍是默认口令，必须先改掉才能使用管理接口。</p>
        <div className="mt-6 space-y-1.5">
          <Label htmlFor="np">新密码</Label>
          <Input id="np" type="password" value={pw} onChange={(e) => setPw(e.target.value)} />
        </div>
        <Button type="submit" className="mt-4 w-full" disabled={busy || pw.length < 4}>
          保存并进入
        </Button>
      </form>
    </div>
  );
}
