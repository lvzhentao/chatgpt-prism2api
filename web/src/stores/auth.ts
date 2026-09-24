import { create } from "zustand";

export const useAuthStore = create<{
  username: string;
  mustChange: boolean;
  setSession: (username: string, mustChange: boolean) => void;
  clear: () => void;
}>((set) => ({
  username: "",
  mustChange: false,
  setSession: (username, mustChange) => set({ username, mustChange }),
  clear: () => set({ username: "", mustChange: false }),
}));
