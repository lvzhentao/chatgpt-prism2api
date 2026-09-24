import { QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { lazy, Suspense, type ReactNode } from "react";
import { createBrowserRouter, Navigate, RouterProvider } from "react-router-dom";
import { Toaster } from "sonner";
import { RequireAuth } from "@/components/auth/require-auth";
import { ErrorBoundary } from "@/components/error-boundary";
import { AppShell } from "@/components/layout/app-shell";
import { PageSkeleton } from "@/components/shared/page-skeleton";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ApiError } from "@/lib/api";
import { useThemeStore } from "@/stores/theme";

const LoginPage = lazy(() => import("@/pages/login-page").then((m) => ({ default: m.LoginPage })));
const PasswordPage = lazy(() => import("@/pages/password-page").then((m) => ({ default: m.PasswordPage })));
const OverviewPage = lazy(() => import("@/pages/overview-page").then((m) => ({ default: m.OverviewPage })));
const AccountsPage = lazy(() => import("@/pages/accounts-page").then((m) => ({ default: m.AccountsPage })));
const AccountDetailPage = lazy(() => import("@/pages/account-detail-page").then((m) => ({ default: m.AccountDetailPage })));
const KeysPage = lazy(() => import("@/pages/keys-page").then((m) => ({ default: m.KeysPage })));
const KeyDetailPage = lazy(() => import("@/pages/key-detail-page").then((m) => ({ default: m.KeyDetailPage })));
const GroupsPage = lazy(() => import("@/pages/groups-page").then((m) => ({ default: m.GroupsPage })));
const GroupDetailPage = lazy(() => import("@/pages/group-detail-page").then((m) => ({ default: m.GroupDetailPage })));
const LogsPage = lazy(() => import("@/pages/logs-page").then((m) => ({ default: m.LogsPage })));
const LogDetailPage = lazy(() => import("@/pages/log-detail-page").then((m) => ({ default: m.LogDetailPage })));
const TasksPage = lazy(() => import("@/pages/tasks-page").then((m) => ({ default: m.TasksPage })));
const TaskDetailPage = lazy(() => import("@/pages/task-detail-page").then((m) => ({ default: m.TaskDetailPage })));
const UsagePage = lazy(() => import("@/pages/usage-page").then((m) => ({ default: m.UsagePage })));
const ProxiesPage = lazy(() => import("@/pages/proxies-page").then((m) => ({ default: m.ProxiesPage })));
const ProxyDetailPage = lazy(() => import("@/pages/proxy-detail-page").then((m) => ({ default: m.ProxyDetailPage })));
const ConfigPage = lazy(() => import("@/pages/config-page").then((m) => ({ default: m.ConfigPage })));
const EmulationPage = lazy(() => import("@/pages/emulation-page").then((m) => ({ default: m.EmulationPage })));
const ModelsPage = lazy(() => import("@/pages/models-page").then((m) => ({ default: m.ModelsPage })));
const BatchesPage = lazy(() => import("@/pages/batches-page").then((m) => ({ default: m.BatchesPage })));
const BatchDetailPage = lazy(() => import("@/pages/batch-detail-page").then((m) => ({ default: m.BatchDetailPage })));

const queryClient = new QueryClient({
  queryCache: new QueryCache({
    onError: (err) => {
      if (!(err instanceof ApiError) || err.status !== 401) return;
      if (window.location.pathname.includes("/login")) return;
      window.location.assign("/admin/login");
    },
  }),
  defaultOptions: {
    queries: {
      retry: (n, err) => (err instanceof ApiError && (err.status === 401 || err.status === 403) ? false : n < 1),
      refetchOnWindowFocus: false,
    },
  },
});

function Fallback() {
  return <PageSkeleton />;
}

function page(el: ReactNode) {
  return <Suspense fallback={<Fallback />}>{el}</Suspense>;
}

const router = createBrowserRouter(
  [
    { path: "/login", element: page(<LoginPage />) },
    { path: "/password", element: page(<PasswordPage />) },
    {
      element: <RequireAuth />,
      children: [
        {
          element: <AppShell />,
          children: [
            {
              path: "/",
              element: page(<OverviewPage />),
              handle: { crumbs: [{ label: "总览" }, { label: "工作台", to: "/" }] },
            },
            {
              path: "/accounts",
              handle: { crumbs: [{ label: "资源" }, { label: "账号", to: "/accounts" }] },
              children: [
                { index: true, element: page(<AccountsPage />) },
                {
                  path: ":name",
                  element: page(<AccountDetailPage />),
                  handle: { crumb: (p: { name?: string }) => ({ label: p.name || "详情" }) },
                },
              ],
            },
            {
              path: "/keys",
              handle: { crumbs: [{ label: "资源" }, { label: "入口密钥", to: "/keys" }] },
              children: [
                { index: true, element: page(<KeysPage />) },
                {
                  path: ":id",
                  element: page(<KeyDetailPage />),
                  handle: { crumb: (p: { id?: string }) => ({ label: p.id || "详情" }) },
                },
              ],
            },
            {
              path: "/groups",
              handle: { crumbs: [{ label: "资源" }, { label: "分组", to: "/groups" }] },
              children: [
                { index: true, element: page(<GroupsPage />) },
                {
                  path: ":name",
                  element: page(<GroupDetailPage />),
                  handle: { crumb: (p: { name?: string }) => ({ label: p.name || "详情" }) },
                },
              ],
            },
            {
              path: "/logs",
              handle: { crumbs: [{ label: "可观测" }, { label: "请求日志", to: "/logs" }] },
              children: [
                { index: true, element: page(<LogsPage />) },
                {
                  path: ":id",
                  element: page(<LogDetailPage />),
                  handle: { crumb: (p: { id?: string }) => ({ label: p.id || "详情" }) },
                },
              ],
            },
            {
              path: "/tasks",
              handle: { crumbs: [{ label: "可观测" }, { label: "任务中心", to: "/tasks" }] },
              children: [
                { index: true, element: page(<TasksPage />) },
                {
                  path: ":id",
                  element: page(<TaskDetailPage />),
                  handle: { crumb: (p: { id?: string }) => ({ label: p.id || "详情" }) },
                },
              ],
            },
            {
              path: "/usage",
              element: page(<UsagePage />),
              handle: { crumbs: [{ label: "可观测" }, { label: "用量", to: "/usage" }] },
            },
            {
              path: "/proxies",
              handle: { crumbs: [{ label: "出口" }, { label: "代理池", to: "/proxies" }] },
              children: [
                { index: true, element: page(<ProxiesPage />) },
                {
                  path: ":id",
                  element: page(<ProxyDetailPage />),
                  handle: { crumb: (p: { id?: string }) => ({ label: p.id || "详情" }) },
                },
              ],
            },
            {
              path: "/config",
              element: page(<ConfigPage />),
              handle: { crumbs: [{ label: "配置" }, { label: "配置中心", to: "/config" }] },
            },
            {
              path: "/emulation",
              element: page(<EmulationPage />),
              handle: { crumbs: [{ label: "配置" }, { label: "协议外观", to: "/emulation" }] },
            },
            {
              path: "/models",
              element: page(<ModelsPage />),
              handle: { crumbs: [{ label: "配置" }, { label: "模型目录", to: "/models" }] },
            },
            {
              path: "/batches",
              handle: { crumbs: [{ label: "配置" }, { label: "消息批次", to: "/batches" }] },
              children: [
                { index: true, element: page(<BatchesPage />) },
                {
                  path: ":id",
                  element: page(<BatchDetailPage />),
                  handle: { crumb: (p: { id?: string }) => ({ label: p.id || "详情" }) },
                },
              ],
            },
            { path: "*", element: <Navigate to="/" replace /> },
          ],
        },
      ],
    },
  ],
  { basename: "/admin" },
);

function ThemedToaster() {
  const theme = useThemeStore((s) => s.theme);
  return (
    <Toaster
      position="top-right"
      theme={theme}
      toastOptions={{
        classNames: {
          toast: "bg-bone text-ink border border-border shadow-[var(--shadow-flyout)]",
        },
      }}
    />
  );
}

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <ErrorBoundary>
          <RouterProvider router={router} />
        </ErrorBoundary>
        <ThemedToaster />
      </TooltipProvider>
    </QueryClientProvider>
  );
}
