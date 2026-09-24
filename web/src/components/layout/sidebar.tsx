import {
  Activity,
  Boxes,
  FolderKey,
  Gauge,
  KeyRound,
  LayoutDashboard,
  Layers3,
  Network,
  ScrollText,
  Settings2,
  Shield,
  Shuffle,
  Users,
  Workflow,
} from "lucide-react";
import { NavLink } from "react-router-dom";
import { brand } from "@/lib/brand";
import { cn } from "@/lib/utils";

type NavItem = {
  to: string;
  label: string;
  icon: typeof LayoutDashboard;
  end?: boolean;
  /** 外链（服务端内嵌页）新窗口打开，不走 React 路由 */
  external?: boolean;
};

const groups: { title: string; items: NavItem[] }[] = [
  {
    title: "总览",
    items: [{ to: "/", label: "工作台", icon: LayoutDashboard, end: true }],
  },
  {
    title: "资源",
    items: [
      { to: "/accounts", label: "账号", icon: Users },
      { to: "/api/admin/pool/view", label: "账号池", icon: FolderKey, external: true },
      { to: "/keys", label: "入口密钥", icon: KeyRound },
      { to: "/groups", label: "分组", icon: Layers3 },
    ],
  },
  {
    title: "可观测",
    items: [
      { to: "/api/admin/metrics/view", label: "实时容量", icon: Gauge, external: true },
      { to: "/logs", label: "请求日志", icon: ScrollText },
      { to: "/tasks", label: "任务中心", icon: Workflow },
      { to: "/usage", label: "用量", icon: Activity },
    ],
  },
  {
    title: "出口",
    items: [{ to: "/proxies", label: "代理池", icon: Network }],
  },
  {
    title: "配置",
    items: [
      { to: "/config", label: "配置中心", icon: Settings2 },
      { to: "/emulation", label: "协议外观", icon: Shield },
      { to: "/models", label: "模型目录", icon: Boxes },
      { to: "/batches", label: "消息批次", icon: Shuffle },
    ],
  },
];

export function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  return (
    <aside className="flex h-full w-[224px] shrink-0 flex-col border-r border-border bg-parchment px-3 py-5">
      <div className="mb-6 flex items-center gap-2.5 px-2">
        <div
          className="grid h-8 w-8 shrink-0 place-items-center rounded-md text-[11px] font-semibold text-white"
          style={{ background: brand.primaryColor }}
        >
          {brand.logoText}
        </div>
        <div className="min-w-0 leading-tight">
          <div className="truncate text-[14px] font-medium tracking-[-0.01em] text-ink">{brand.appName}</div>
          <div className="font-mono text-[11px] text-ash">{brand.version}</div>
        </div>
      </div>
      <nav className="flex flex-1 flex-col gap-5 overflow-y-auto">
        {groups.map((g) => (
          <div key={g.title}>
            <div className="mb-1 px-2 font-mono text-[11px] text-ash">{g.title}</div>
            <div className="flex flex-col gap-0.5">
              {g.items.map((item) =>
                item.external ? (
                  <a
                    key={item.to}
                    href={item.to}
                    target="_blank"
                    rel="noopener"
                    onClick={onNavigate}
                    className="flex items-center gap-2.5 rounded-md px-2.5 py-2 text-[13.5px] text-ash transition-colors duration-150 hover:bg-linen hover:text-ink"
                  >
                    <item.icon className="size-4" />
                    {item.label}
                  </a>
                ) : (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    onClick={onNavigate}
                    className={({ isActive }) =>
                      cn(
                        "flex items-center gap-2.5 rounded-md px-2.5 py-2 text-[13.5px] text-ash transition-colors duration-150",
                        isActive && "bg-linen text-ink",
                      )
                    }
                  >
                    <item.icon className="size-4" />
                    {item.label}
                  </NavLink>
                ),
              )}
            </div>
          </div>
        ))}
      </nav>
    </aside>
  );
}
