export type SSEHandler = (event: string, data: unknown) => void;
export type SSEStatus = "connecting" | "live" | "retry" | "closed";

export function openSSE(
  url: string,
  onEvent: SSEHandler,
  onError?: (err: unknown) => void,
  onStatus?: (status: SSEStatus) => void,
) {
  let stopped = false;
  let attempt = 0;
  let timer = 0;
  let ctrl: AbortController | null = null;

  const connect = async () => {
    if (stopped) return;
    ctrl = new AbortController();
    onStatus?.(attempt === 0 ? "connecting" : "retry");
    try {
      const res = await fetch(url, {
        credentials: "include",
        headers: { Accept: "text/event-stream" },
        signal: ctrl.signal,
      });
      if (!res.ok || !res.body) {
        throw new Error(`SSE ${res.status}`);
      }
      attempt = 0;
      onStatus?.("live");
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      while (!stopped) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const blocks = buf.split("\n\n");
        buf = blocks.pop() ?? "";
        for (const block of blocks) {
          let event = "message";
          const dataLines: string[] = [];
          for (const line of block.split("\n")) {
            if (line.startsWith("event:")) event = line.slice(6).trim();
            else if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
          }
          if (dataLines.length === 0) continue;
          const raw = dataLines.join("\n");
          try {
            onEvent(event, JSON.parse(raw));
          } catch {
            onEvent(event, raw);
          }
        }
      }
      if (!stopped) throw new Error("SSE ended");
    } catch (err) {
      if (stopped || (err as { name?: string }).name === "AbortError") return;
      onError?.(err);
      const delay = Math.min(800 * 2 ** attempt, 8000);
      attempt += 1;
      onStatus?.("retry");
      timer = window.setTimeout(() => void connect(), delay);
    }
  };

  void connect();
  return () => {
    stopped = true;
    onStatus?.("closed");
    window.clearTimeout(timer);
    ctrl?.abort();
  };
}
