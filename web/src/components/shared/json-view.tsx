export function JsonView({ value }: { value: unknown }) {
  const text = typeof value === "string" ? value : JSON.stringify(value, null, 2);
  return (
    <pre className="max-h-[480px] overflow-auto rounded-md border border-border bg-parchment p-3 font-mono text-[12px] leading-relaxed text-ink">
      {text || "—"}
    </pre>
  );
}
