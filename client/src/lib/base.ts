const STORAGE_KEY = "wacalls.base_url";

export const BASE_URL_STORAGE_KEY = STORAGE_KEY;

export type NormalizeResult =
  | { ok: true; value: string }
  | { ok: false; error: string };

export type ResolvedBase = {
  value: string;
  source: "panel" | "build" | "fallback";
};

// In-memory override used when localStorage is unavailable (session-only).
let sessionBase: string | null = null;

/**
 * Validate and normalize a user/build-time base URL.
 * Allowlist: "" (same-origin) | single-leading-slash path | absolute https: (http: only on http pages).
 * Mirrors the SDK's own normalization in sdk/src/index.ts (trailing slash strip + simple concatenation).
 */
export function normalizeBaseUrl(raw: string, protocol: string = (typeof window !== "undefined" ? window.location.protocol : "http:")): NormalizeResult {
  const trimmed = raw.trim();
  if (trimmed === "") return { ok: true, value: "" };

  // absolute URL
  if (/^https?:\/\//i.test(trimmed)) {
    let u: URL;
    try {
      u = new URL(trimmed);
    } catch {
      return { ok: false, error: "invalid URL" };
    }
    if (u.protocol === "http:" && protocol === "https:") {
      return { ok: false, error: "http:// base is not allowed on an https page" };
    }
    if (u.search || u.hash) {
      return { ok: false, error: "no query or hash allowed" };
    }
    if (/[^\x20-\x7e]/.test(trimmed) || /\s/.test(trimmed)) {
      return { ok: false, error: "invalid characters" };
    }
    return { ok: true, value: u.origin + u.pathname.replace(/\/+$/, "") };
  }

  // path-only
  if (!trimmed.startsWith("/")) {
    return { ok: false, error: "Use a path like /service-endpoint-wacalls or a full https:// URL." };
  }
  if (trimmed.startsWith("//")) {
    return { ok: false, error: "protocol-relative URLs (//...) are not allowed" };
  }
  if (/[?#\s\\]/.test(trimmed)) {
    return { ok: false, error: "no query, hash, whitespace, or backslash allowed" };
  }
  return { ok: true, value: trimmed.replace(/\/+$/, "") };
}

/**
 * Resolve the effective base URL. Read at call time (no caching) so panel
 * edits apply immediately. Fallback chain: in-memory session override →
 * localStorage → build-time env → "".
 */
export function resolveBaseUrl(): string {
  return resolveBaseUrlDetailed().value;
}

export function resolveBaseUrlDetailed(): ResolvedBase {
  if (sessionBase !== null) return { value: sessionBase, source: "panel" };
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored !== null) {
      const n = normalizeBaseUrl(stored);
      if (n.ok) return { value: n.value, source: "panel" };
    }
  } catch {
    // ignore
  }
  const env = import.meta.env.VITE_WACALLS_BASE_URL;
  if (typeof env === "string" && env.trim() !== "") {
    const n = normalizeBaseUrl(env);
    if (n.ok) return { value: n.value, source: "build" };
  }
  return { value: "", source: "fallback" };
}

/**
 * Join a base URL with an API path. Identity when base is "".
 * Mirrors SDK semantics: simple concatenation after trailing-slash strip.
 */
export function withBase(path: string): string {
  const base = resolveBaseUrl();
  if (!base) return path;
  return path.startsWith("/") ? `${base}${path}` : `${base}/${path}`;
}

export function persistBaseUrl(raw: string): { ok: true; sessionOnly?: false } | { ok: true; sessionOnly: true } | { ok: false; reason: "validation" } {
  const n = normalizeBaseUrl(raw);
  if (!n.ok) return { ok: false, reason: "validation" };
  // Empty input clears the override (no shadowing of build default).
  if (n.value === "") {
    clearBaseUrl();
    return { ok: true };
  }
  try {
    localStorage.setItem(STORAGE_KEY, n.value);
    sessionBase = null;
  } catch {
    sessionBase = n.value;
    return { ok: true, sessionOnly: true };
  }
  return { ok: true };
}

export function isSessionOnlyBase(): boolean {
  return sessionBase !== null;
}

export function clearBaseUrl(): void {
  sessionBase = null;
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}
