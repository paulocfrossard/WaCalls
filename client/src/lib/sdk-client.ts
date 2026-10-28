import { WaCallsClient } from "wacalls-sdk";
import { getClientId } from "@/lib/client-id";
import { getToken, useAuth } from "@/stores/auth";
import { useDevices } from "@/stores/devices";

/**
 * The app's single WaCallsClient instance — sole owner of SSE, REST call
 * actions, WebRTC/media and the downlink <audio>. Same-origin by default
 * (VITE_WACALLS_BASE_URL can point at another origin), reusing the persisted
 * client id (X-Client-Id + SSE clientId).
 *
 * Auth is NOT read from build-time env: the session token lives in memory in
 * the auth store and is pushed into the SDK for REST `Authorization: Bearer`.
 * The SSE stream never carries the session token: it authenticates with a
 * short-lived single-use ticket fetched per (re)connect, or the HttpOnly
 * cookie when ticket acquisition fails.
 */
export const sdkClient = new WaCallsClient({
  baseUrl: import.meta.env.VITE_WACALLS_BASE_URL ?? "",
  clientId: getClientId(),
  // Ticket is the ONLY SSE credential: no apiKey is ever appended to the
  // events URL (API keys stay REST-only). On failure the SDK falls back to the
  // HttpOnly cookie with no query credential.
  ticketProvider: async () => {
    const r = await fetch("/api/auth/sse-ticket", { method: "POST", credentials: "include" });
    if (!r.ok) throw new Error("sse ticket");
    return (await r.json()).ticket;
  },
});

// Seed with the persisted token, then follow auth changes (login/logout).
sdkClient.setToken(getToken());

useAuth.subscribe((state, prev) => {
  if (state.token === prev.token) return;
  sdkClient.setToken(getToken());
  // A fresh login must reopen the stream even if it was never opened (or was
  // closed by a previous logout); connect() is idempotent.
  if (getToken() !== null) sdkClient.connect();
});

// Keep the SDK's device selection in sync with the devices store: micId applies
// to the next call opened; outId also re-routes the active downlink immediately.
useDevices.subscribe((state, prev) => {
  if (state.micId !== prev.micId) sdkClient.setMicId(state.micId);
  if (state.outId !== prev.outId) sdkClient.setOutId(state.outId);
});
