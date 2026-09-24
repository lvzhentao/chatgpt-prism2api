import { Link } from "react-router-dom";
import { Button } from "@/components/ui/button";

export function confirmDelete(label: string) {
  return window.confirm(`删除${label}？此操作不能恢复。`);
}

export function RowActions({
  to,
  onDelete,
  deleteDisabled,
  deleteHint,
}: {
  to: string;
  onDelete?: () => void | Promise<void>;
  deleteDisabled?: boolean;
  deleteHint?: string;
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <Button variant="ember" size="sm" asChild>
        <Link to={to}>编辑</Link>
      </Button>
      {onDelete ? (
        <Button variant="destructive" size="sm" disabled={deleteDisabled} title={deleteHint} onClick={() => void onDelete()}>
          删除
        </Button>
      ) : null}
    </div>
  );
}
