import { WaCallsClient } from "wacalls-sdk";
import { getClientId } from "@/lib/client-id";
import { useDevices } from "@/stores/devices";

/**
 * The app's single WaCallsClient instance — sole owner of SSE, REST call
 * actions, WebRTC/media and the downlink <audio>. Same-origin (baseUrl ""),
 * reusing the persisted client id (X-Client-Id + SSE clientId).
 */
export const sdkClient = new WaCallsClient({
  baseUrl: "",
  clientId: getClientId(),
  apiKey: import.meta.env.VITE_WACALLS_API_KEY ?? "",
});

// Keep the SDK's device selection in sync with the devices store: micId applies
// to the next call opened; outId also re-routes the active downlink immediately.
useDevices.subscribe((state, prev) => {
  if (state.micId !== prev.micId) sdkClient.setMicId(state.micId);
  if (state.outId !== prev.outId) sdkClient.setOutId(state.outId);
});
