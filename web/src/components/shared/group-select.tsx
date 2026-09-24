import { Checkbox } from "@/components/ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { cn } from "@/lib/utils";
import type { Group } from "@/types/api";

const NONE = "__none__";

export function GroupSelect({
  value,
  groups,
  onChange,
  placeholder = "不绑定分组",
  className,
}: {
  value?: string;
  groups: Group[];
  onChange: (name: string) => void;
  placeholder?: string;
  className?: string;
}) {
  return (
    <Select value={value || NONE} onValueChange={(v) => onChange(v === NONE ? "" : v)}>
      <SelectTrigger className={className}>
        <SelectValue placeholder={placeholder} />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={NONE}>{placeholder}</SelectItem>
        {groups.map((g) => (
          <SelectItem key={g.name} value={g.name}>
            {g.name}
            {g.description ? ` · ${g.description}` : ""}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

export function GroupChecks({
  value,
  groups,
  extras = [],
  onChange,
}: {
  value: string[];
  groups: Group[];
  extras?: string[];
  onChange: (next: string[]) => void;
}) {
  const names = [...new Set([...groups.map((g) => g.name), ...extras])].filter(Boolean);
  if (names.length === 0) {
    return <p className="text-[12px] text-ash">还没有分组。先到「分组」建一个，再回来勾选。</p>;
  }
  return (
    <div className="flex flex-wrap gap-2">
      {names.map((name) => {
        const checked = value.includes(name);
        const desc = groups.find((g) => g.name === name)?.description;
        return (
          <label
            key={name}
            className={cn(
              "inline-flex items-center gap-2 rounded-md border border-border bg-bone px-2.5 py-1.5 text-[13px]",
              checked && "border-ink/30",
            )}
          >
            <Checkbox
              checked={checked}
              onCheckedChange={(v) => onChange(v === true ? [...value, name] : value.filter((x) => x !== name))}
            />
            <span>
              {name}
              {desc ? <span className="ml-1 text-ash">{desc}</span> : null}
            </span>
          </label>
        );
      })}
    </div>
  );
}
