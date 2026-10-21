import { create } from "zustand";

/**
 * Top-level view state. The app has no router (see `App.tsx`): navigation is
 * plain state, so the DEBUG panel is a view like any other content block.
 */
export type AppView = "app" | "debug";

type State = {
  view: AppView;
  setView: (view: AppView) => void;
};

export const useUi = create<State>((set) => ({
  view: "app",
  setView: (view) => set({ view }),
}));
