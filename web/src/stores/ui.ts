import { create } from "zustand";

type AccountView = "card" | "list";

export const useUIStore = create<{
  accountView: AccountView;
  sidebarOpen: boolean;
  setAccountView: (v: AccountView) => void;
  setSidebarOpen: (v: boolean) => void;
}>((set) => ({
  accountView: (localStorage.getItem("c2a-account-view") as AccountView) || "card",
  sidebarOpen: false,
  setAccountView: (accountView) => {
    localStorage.setItem("c2a-account-view", accountView);
    set({ accountView });
  },
  setSidebarOpen: (sidebarOpen) => set({ sidebarOpen }),
}));
