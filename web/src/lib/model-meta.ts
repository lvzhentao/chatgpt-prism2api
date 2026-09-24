export type Vendor = {
  id: string;
  label: string;
  icon: string;
};

const vendors: { test: RegExp; vendor: Vendor }[] = [
  { test: /claude|anthropic|sonnet|opus|haiku|fable/, vendor: { id: "anthropic", label: "Anthropic", icon: "claude" } },
  { test: /gpt|openai|^o[1-4]\b|chatgpt/, vendor: { id: "openai", label: "OpenAI", icon: "openai" } },
  { test: /gemini|gemma|google/, vendor: { id: "google", label: "Google", icon: "gemini" } },
  { test: /grok|xai/, vendor: { id: "xai", label: "xAI", icon: "grok" } },
  { test: /deepseek/, vendor: { id: "deepseek", label: "DeepSeek", icon: "deepseek" } },
  { test: /qwen|qwq/, vendor: { id: "qwen", label: "Qwen", icon: "qwen" } },
  { test: /kimi|moonshot/, vendor: { id: "moonshot", label: "Moonshot", icon: "kimi" } },
  { test: /glm|chatglm|zhipu/, vendor: { id: "zhipu", label: "智谱", icon: "zhipu" } },
  { test: /llama|meta-/, vendor: { id: "meta", label: "Meta", icon: "meta" } },
  { test: /mistral|mixtral|codestral/, vendor: { id: "mistral", label: "Mistral", icon: "mistral" } },
  { test: /composer|vendor/, vendor: { id: "vendor", label: "Vendor", icon: "lobehub" } },
];

const vendorRank: Record<string, number> = {
  anthropic: 0,
  openai: 1,
  google: 2,
  xai: 3,
  deepseek: 4,
  qwen: 5,
  moonshot: 6,
  zhipu: 7,
  meta: 8,
  mistral: 9,
  vendor: 10,
  other: 99,
};

export function vendorOf(id: string): Vendor {
  const key = id.toLowerCase();
  for (const row of vendors) {
    if (row.test.test(key)) return row.vendor;
  }
  return { id: "other", label: "其他", icon: "lobehub" };
}

export function vendorSort(a: string, b: string) {
  return (vendorRank[a] ?? 50) - (vendorRank[b] ?? 50) || a.localeCompare(b);
}

// OpenAI / Grok 等只有单色 SVG，没有 *-color.svg。
const colorIcons = new Set(["claude", "gemini", "deepseek", "qwen", "kimi", "zhipu", "meta", "mistral", "lobehub"]);

export function lobeIconFile(slug: string, color?: boolean) {
  const useColor = color ?? colorIcons.has(slug);
  return useColor ? `${slug}-color.svg` : `${slug}.svg`;
}

export function lobeIconSrc(slug: string, color?: boolean) {
  return `https://unpkg.com/@lobehub/icons-static-svg@1.74.0/icons/${lobeIconFile(slug, color)}`;
}

export function lobeIconFallback(slug: string, color?: boolean) {
  return `https://registry.npmmirror.com/@lobehub/icons-static-svg/1.74.0/files/icons/${lobeIconFile(slug, color)}`;
}
