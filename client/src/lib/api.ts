import { getClientId } from "./client-id";
import { sdkClient } from "./sdk-client";
import { getToken, isBootstrapping, useAuth } from "@/stores/auth";

const baseHeaders = (): Record<string, string> => {
  const headers: Record<string, string> = {
    "X-Client-Id": getClientId(),
    "Content-Type": "application/json",
  };
  const token = getToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;
  return headers;
};

/** REST failure with the HTTP status attached (lets callers react to 401). */
export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = "ApiError";
  }
}

/**
 * A 401 means the session is gone (expired/revoked): end any live call with
 * the token we still have, drop local auth and hard-navigate to the login
 * gate. Guarded so a 401 already on /login does not reload the page in a loop.
 */
let redirecting = false;

const redirectToLogin = (): void => {
  // Concurrent 401s must not run the teardown/navigation more than once.
  if (redirecting) return;
  redirecting = true;
  void (async () => {
    try {
      await sdkClient.hangupActive();
    } catch {
      // Best effort: local session teardown must still run.
    } finally {
      useAuth.getState().logout();
      if (typeof window !== "undefined" && window.location.pathname !== "/login") {
        window.location.assign("/login");
      }
    }
  })();
};

/**
 * Single entry point for authenticated REST calls: adds X-Client-Id, the
 * in-memory session bearer token (when present) and `credentials: "include"`
 * (HttpOnly cookie session), and signs the user out on 401 — except while the
 * boot-time `bootstrapAuth` probe is still resolving, when a 401 is expected
 * for a logged-out visitor and must not trigger a redirect.
 */
export const apiFetch = async (path: string, init: RequestInit = {}): Promise<Response> => {
  const headers = new Headers(init.headers);
  for (const [key, value] of Object.entries(baseHeaders())) {
    if (!headers.has(key)) headers.set(key, value);
  }
  const r = await fetch(path, { ...init, headers, credentials: "include" });
  if (r.status === 401) {
    if (!isBootstrapping()) redirectToLogin();
    throw new ApiError(401, `${path} ${r.status}`);
  }
  return r;
};

export const apiGet = async <T>(path: string): Promise<T> => {
  const r = await apiFetch(path);
  if (!r.ok) throw new ApiError(r.status, `${path} ${r.status}`);
  return r.json() as Promise<T>;
};

export const apiPost = async <T>(path: string, body: unknown): Promise<T> => {
  const r = await apiFetch(path, { method: "POST", body: JSON.stringify(body) });
  if (!r.ok) {
    const text = await r.text().catch(() => "");
    throw new ApiError(r.status, `${path} ${r.status} ${text}`);
  }
  return r.json() as Promise<T>;
};

export const apiDelete = async (path: string): Promise<void> => {
  const r = await apiFetch(path, { method: "DELETE" });
  if (!r.ok) throw new ApiError(r.status, `${path} ${r.status}`);
};
