export function formatTime(input?: string | number | Date | null) {
  if (input == null || input === "") return "—";
  const d = typeof input === "number" ? new Date(input > 1e12 ? input : input * 1000) : new Date(input);
  if (Number.isNaN(d.getTime())) return "—";
  return new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  }).format(d);
}

export function formatDuration(seconds?: number) {
  if (seconds == null || seconds < 0) return "—";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `${h} 小时 ${m} 分`;
  if (m > 0) return `${m} 分 ${s} 秒`;
  return `${s} 秒`;
}

export function formatNumber(n?: number | null) {
  if (n == null) return "0";
  return new Intl.NumberFormat("zh-CN").format(n);
}

export function formatPercent(n?: number | null) {
  if (n == null) return "—";
  return `${(n * 100).toFixed(1)}%`;
}

export function formatUSDFromCents(cents?: number | null) {
  if (cents == null || Number.isNaN(cents)) return "—";
  return `$${(cents / 100).toFixed(2)}`;
}

export function formatDisableReason(reason?: string | null) {
  switch (reason) {
    case "quota_api":
      return "API 额度已满";
    case "quota_auto":
      return "Auto + Composer 额度已满";
    case "quota_exhausted":
      return "额度已满";
    case "quota_plan":
      return "套餐额度已满";
    case "quota":
      return "上游额度已尽";
    default:
      return reason || "";
  }
}

export function formatReset(input?: string | number | Date | null) {
  if (input == null || input === "") return "";
  const d = typeof input === "number" ? new Date(input > 1e12 ? input : input * 1000) : new Date(input);
  if (Number.isNaN(d.getTime())) return "";
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  }).format(d);
}

export function statusTone(status?: number) {
  if (!status) return "muted";
  if (status < 400) return "ok";
  if (status < 500) return "warn";
  return "err";
}
