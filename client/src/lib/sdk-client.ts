import { WaCallsClient } from "wacalls-sdk";
import { getClientId } from "@/lib/client-id";
import { useDevices } from "@/stores/devices";
import { BASE_URL_STORAGE_KEY, resolveBaseUrl } from "@/lib/base";

/**
 * The app's single WaCallsClient instance — sole owner of SSE, REST call
 * actions, WebRTC/media and the downlink <audio>. Base URL comes from
 * resolveBaseUrl() (in-memory session override (storage failure) →
 * localStorage → VITE_WACALLS_BASE_URL → "").
 */
export const sdkClient = new WaCallsClient({
  baseUrl: resolveBaseUrl(),
  clientId: getClientId(),
  apiKey: import.meta.env.VITE_WACALLS_API_KEY ?? "",
});

export function reconfigureSdk(baseUrl: string): void {
  sdkClient.setBaseUrl(baseUrl);
}

if (typeof window !== "undefined") {
  window.addEventListener("storage", (e) => {
    if (e.storageArea !== localStorage) return;
    if (e.key !== null && e.key !== BASE_URL_STORAGE_KEY) return;
    reconfigureSdk(resolveBaseUrl());
  });

  // End the active call server-side when the tab is closed/navigated away,
  // instead of waiting for the ICE timeout. disconnect() fires a keepalive
  // DELETE for the owned call before local teardown.
  window.addEventListener("pagehide", (e) => {
    // Skip bfcache transitions: the page may be restored, and disconnect()
    // permanently stops the client. A real unload has persisted === false.
    if (e.persisted) return;
    sdkClient.disconnect();
  });
  window.addEventListener("pageshow", (e) => {
    // Restored from bfcache: the frozen EventSource may be dead, so force a
    // fresh stream (without tearing down the active call).
    if (e.persisted) sdkClient.reconnectStream();
  });
}

// Keep the SDK's device selection in sync with the devices store: micId applies
// to the next call opened; outId also re-routes the active downlink immediately.
useDevices.subscribe((state, prev) => {
  if (state.micId !== prev.micId) sdkClient.setMicId(state.micId);
  if (state.outId !== prev.outId) sdkClient.setOutId(state.outId);
});
