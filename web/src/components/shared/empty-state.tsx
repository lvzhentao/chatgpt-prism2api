import type { ReactNode } from "react";

export function EmptyState({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-start gap-3 py-10">
      <p className="font-serif text-[17px] text-ink">{title}</p>
      {action}
    </div>
  );
}
