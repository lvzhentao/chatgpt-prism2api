import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { openSSE } from "@/lib/sse";

export function LiveEvents() {
  const qc = useQueryClient();
  useEffect(() => {
    const stopLogs = openSSE("/api/admin/events", (event) => {
      if (event === "log" || event === "message") {
        void qc.invalidateQueries({ queryKey: ["logs"] });
        void qc.invalidateQueries({ queryKey: ["dashboard"] });
        void qc.invalidateQueries({ queryKey: ["stats"] });
      }
      if (event === "cooldown" || event === "login") {
        void qc.invalidateQueries({ queryKey: ["accounts"] });
        void qc.invalidateQueries({ queryKey: ["dashboard"] });
      }
    });
    const stopTasks = openSSE("/api/admin/tasks/events", () => {
      void qc.invalidateQueries({ queryKey: ["tasks"] });
    });
    return () => {
      stopLogs();
      stopTasks();
    };
  }, [qc]);
  return null;
}
