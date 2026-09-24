import { create } from "zustand";

type Theme = "light" | "dark";

function apply(theme: Theme) {
  document.documentElement.classList.toggle("dark", theme === "dark");
}

export const useThemeStore = create<{
  theme: Theme;
  toggle: () => void;
  set: (t: Theme) => void;
}>((set, get) => ({
  theme: (localStorage.getItem("c2a-theme") as Theme) || "light",
  set: (theme) => {
    localStorage.setItem("c2a-theme", theme);
    apply(theme);
    set({ theme });
  },
  toggle: () => get().set(get().theme === "light" ? "dark" : "light"),
}));

apply((localStorage.getItem("c2a-theme") as Theme) || "light");
