export async function mapPool<T, R>(items: T[], limit: number, fn: (item: T, i: number) => Promise<R>) {
  const n = Math.max(1, limit || 4);
  const out: R[] = new Array(items.length);
  let i = 0;
  const workers = Array.from({ length: Math.min(n, items.length) }, async () => {
    while (i < items.length) {
      const cur = i++;
      out[cur] = await fn(items[cur], cur);
    }
  });
  await Promise.all(workers);
  return out;
}
