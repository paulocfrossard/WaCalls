import { getClientId } from "./client-id";
import { withBase } from "./base";

const baseHeaders = (): HeadersInit => ({
  "X-Client-Id": getClientId(),
  "Content-Type": "application/json",
});

export const apiGet = async <T>(path: string): Promise<T> => {
  const r = await fetch(withBase(path), { headers: baseHeaders() });
  if (!r.ok) throw new Error(`${path} ${r.status}`);
  return r.json() as Promise<T>;
};

export const apiPost = async <T>(path: string, body: unknown): Promise<T> => {
  const r = await fetch(withBase(path), { method: "POST", headers: baseHeaders(), body: JSON.stringify(body) });
  if (!r.ok) {
    const text = await r.text().catch(() => "");
    throw new Error(`${path} ${r.status} ${text}`);
  }
  return r.json() as Promise<T>;
};

export const apiDelete = async (path: string): Promise<void> => {
  const r = await fetch(withBase(path), { method: "DELETE", headers: baseHeaders() });
  if (!r.ok) throw new Error(`${path} ${r.status}`);
};
