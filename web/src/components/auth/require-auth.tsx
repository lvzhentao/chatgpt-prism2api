import { useEffect } from "react";
import { Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Skeleton } from "@/components/ui/skeleton";
import { ApiError, adminApi } from "@/lib/api";
import { useAuthStore } from "@/stores/auth";

export function RequireAuth() {
  const nav = useNavigate();
  const loc = useLocation();
  const setSession = useAuthStore((s) => s.setSession);
  const q = useQuery({
    queryKey: ["me"],
    queryFn: adminApi.me,
    retry: false,
    staleTime: 15_000,
  });

  useEffect(() => {
    if (!q.data) return;
    setSession(q.data.username, Boolean(q.data.must_change_password));
    if (q.data.must_change_password) {
      nav("/password", { replace: true });
    }
  }, [q.data, nav, setSession]);

  useEffect(() => {
    if (!(q.error instanceof ApiError)) return;
    if (q.error.status === 401) {
      nav("/login", { replace: true, state: { from: loc.pathname } });
      return;
    }
    if (q.error.mustChange) {
      nav("/password", { replace: true });
    }
  }, [q.error, nav, loc.pathname]);

  if (q.isLoading) {
    return (
      <div className="p-8">
        <Skeleton className="h-40" />
      </div>
    );
  }
  if (q.error) return null;
  return <Outlet />;
}
