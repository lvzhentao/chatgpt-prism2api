import { Menu, Moon, Sun, Workflow } from "lucide-react";
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { adminApi } from "@/lib/api";
import { useAuthStore } from "@/stores/auth";
import { useThemeStore } from "@/stores/theme";
import { useUIStore } from "@/stores/ui";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent } from "@/components/ui/sheet";
import { LiveEvents } from "@/components/shared/live-events";
import { brand } from "@/lib/brand";
import { Breadcrumbs } from "./breadcrumbs";
import { Sidebar } from "./sidebar";

export function AppShell() {
  const theme = useThemeStore((s) => s.theme);
  const toggle = useThemeStore((s) => s.toggle);
  const username = useAuthStore((s) => s.username);
  const open = useUIStore((s) => s.sidebarOpen);
  const setOpen = useUIStore((s) => s.setSidebarOpen);
  const nav = useNavigate();
  const loc = useLocation();
  const tasks = useQuery({ queryKey: ["tasks"], queryFn: adminApi.tasks, refetchInterval: 8000 });
  const running = (tasks.data?.tasks ?? []).filter((t) => t.status === "running" || t.status === "pending").length;

  return (
    <div className="flex min-h-dvh bg-background">
      <LiveEvents />
      <div className="hidden lg:block">
        <div className="sticky top-0 h-dvh">
          <Sidebar />
        </div>
      </div>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="left" className="p-0">
          <Sidebar onNavigate={() => setOpen(false)} />
        </SheetContent>
      </Sheet>
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-30 flex h-[52px] items-center justify-between gap-3 border-b border-border bg-background/90 px-4 backdrop-blur-sm">
          <div className="flex items-center gap-2">
            <Button variant="ghost" size="icon" className="lg:hidden" onClick={() => setOpen(true)} aria-label="打开导航">
              <Menu />
            </Button>
            <span className="hidden text-[13px] text-ash sm:inline">{username}</span>
          </div>
          <div className="flex items-center gap-2">
            <Link to="/tasks" className="inline-flex h-8 items-center gap-1.5 rounded-md px-2.5 text-[13px] text-foreground/70 hover:underline">
              <Workflow className="size-4" />
              <span className="hidden sm:inline">任务</span>
              {running > 0 ? <span className="font-mono text-[12px] text-ember">{running}</span> : null}
            </Link>
            <Button variant="ghost" size="icon" onClick={toggle} aria-label="切换亮暗色">
              {theme === "dark" ? <Sun /> : <Moon />}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={async () => {
                await adminApi.logout();
                nav("/login");
              }}
            >
              退出
            </Button>
          </div>
        </header>
        <main className="mx-auto w-full max-w-[1360px] flex-1 px-4 py-6 sm:px-6">
          <Breadcrumbs />
          <div key={loc.pathname} className="page-enter">
            <Outlet />
          </div>
          <footer className="mt-10 border-t border-border pt-4 font-mono text-[12px] text-ash">
            {brand.footer}
          </footer>
        </main>
      </div>
    </div>
  );
}
