import { create } from "zustand";

type AuthState = { user: string | null; token: string | null; isAuthenticated: boolean };
type AuthActions = { login: (user: string, token: string) => void; logout: () => void };

const initialToken: string | null = localStorage.getItem("wacalls.token");

export const useAuth = create<AuthState & AuthActions>((set) => ({
  user: null,
  token: initialToken,
  isAuthenticated: initialToken !== null && initialToken.trim() !== "",
  login: (user, token) => {
    localStorage.setItem("wacalls.token", token);
    set({ user, token, isAuthenticated: token.trim() !== "" });
  },
  logout: () => {
    localStorage.removeItem("wacalls.token");
    set({ user: null, token: null, isAuthenticated: false });
  },
}));

export const getToken = (): string | null => {
  const token = useAuth.getState().token;
  if (token === null || token.trim() === "") return null;
  return token;
};

export const ensureAuthWired = (): void => {};
