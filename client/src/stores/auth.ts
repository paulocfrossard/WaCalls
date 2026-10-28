import { create } from "zustand";

type AuthState = {
  user: string | null;
  token: string | null;
  isAuthenticated: boolean;
  /** Session expiry in Unix milliseconds; null when unknown (cookie session). */
  expiresAt: number | null;
};
type AuthActions = {
  login: (user: string, token: string | null, expiresAt: number | null) => void;
  logout: () => void;
};

/** Legacy persisted keys — the session token now lives in memory only. */
const LEGACY_TOKEN_KEY = "wacalls.token";
const LEGACY_EXPIRES_KEY = "wacalls.expiresAt";

/**
 * Remove credentials written by older builds. The token/expiry are no longer
 * persisted (XSS could read a long-lived bearer; replay outlives the tab), so
 * any surviving copy is deleted on boot.
 */
const clearLegacyStorage = (): void => {
  try {
    localStorage.removeItem(LEGACY_TOKEN_KEY);
    localStorage.removeItem(LEGACY_EXPIRES_KEY);
  } catch {
    // Storage can be unavailable (private mode / SSR-ish environments).
  }
};

clearLegacyStorage();

let expiryTimer: ReturnType<typeof setTimeout> | null = null;

const clearExpiryTimer = (): void => {
  if (expiryTimer !== null) {
    clearTimeout(expiryTimer);
    expiryTimer = null;
  }
};

/**
 * Arm (or drop) the auto-logout for a session expiry. An already-expired
 * session is logged out immediately; a live one is logged out at `expiresAt`
 * and sent to the login gate. The timer is always cleared by logout().
 */
const scheduleExpiry = (expiresAt: number | null): void => {
  clearExpiryTimer();
  if (expiresAt === null) return;
  const remaining = expiresAt - Date.now();
  if (remaining <= 0) {
    useAuth.getState().logout();
    return;
  }
  expiryTimer = setTimeout(() => {
    expiryTimer = null;
    useAuth.getState().logout();
    if (typeof window !== "undefined" && window.location.pathname !== "/login") {
      window.location.assign("/login");
    }
  }, remaining);
};

export const useAuth = create<AuthState & AuthActions>((set) => ({
  user: null,
  // Memory only: no token is restored from storage at boot.
  token: null,
  isAuthenticated: false,
  expiresAt: null,
  login: (user, token, expiresAt) => {
    // Cookie sessions may have no token: the HttpOnly cookie authenticates.
    const next = typeof token === "string" && token.trim() !== "" ? token : null;
    set({ user, token: next, isAuthenticated: true, expiresAt });
    scheduleExpiry(expiresAt);
  },
  logout: () => {
    clearExpiryTimer();
    clearLegacyStorage();
    set({ user: null, token: null, isAuthenticated: false, expiresAt: null });
  },
}));

/** Current bearer token, or null when absent/blank (cookie session). */
export const getToken = (): string | null => {
  const token = useAuth.getState().token;
  if (token === null || token.trim() === "") return null;
  return token;
};

let bootstrapPromise: Promise<void> | null = null;

/** True while the boot-time session probe is in flight (401s must not redirect). */
export const isBootstrapping = (): boolean => bootstrapPromise !== null;

/**
 * Restore a cookie session at boot. Without an in-memory token, ask
 * `GET /api/auth/me` (HttpOnly cookie is sent via `credentials: "include"`):
 * a 200 authenticates the user while `token` stays null, so later requests
 * rely on the cookie; a 401 (or a network failure) leaves the user logged out.
 *
 * Idempotent for concurrent callers (React StrictMode runs the mount effect
 * twice) and skipped when a token already lives in memory.
 */
export const bootstrapAuth = (): Promise<void> => {
  if (bootstrapPromise !== null) return bootstrapPromise;

  bootstrapPromise = (async () => {
    if (getToken() !== null) return;
    try {
      const r = await fetch("/api/auth/me", {
        method: "GET",
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      if (!r.ok) return; // 401 (or any failure): stay on the login gate.
      const data = (await r.json()) as { user?: { id?: unknown; username?: unknown } } | null;
      const info = data?.user;
      const user =
        typeof info?.username === "string" && info.username !== ""
          ? info.username
          : typeof info?.id === "string" && info.id !== ""
            ? info.id
            : null;
      useAuth.setState({ user, isAuthenticated: true });
    } catch {
      // Network failure: leave the user logged out; the gate stays usable.
    }
  })().finally(() => {
    bootstrapPromise = null;
  });

  return bootstrapPromise;
};
