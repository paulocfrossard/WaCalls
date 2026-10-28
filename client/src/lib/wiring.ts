import { sdkClient } from "@/lib/sdk-client";
import { queryClient } from "@/lib/query";
import { resetCallsWiring } from "@/stores/calls";
import { resetSessionsWiring } from "@/stores/sessions";

/**
 * Tear down everything auth-scoped: end the active call with the still-valid
 * token, detach the stores' SDK listeners, clear their state/cache and close
 * the SSE stream (disconnect() also releases an active call).
 *
 * Ordering matters: hangupActive() runs BEFORE disconnect() so the DELETE
 * still carries the current bearer token. Callers that also clear the auth
 * store must await this (or hang up explicitly) before logout().
 *
 * Lives here — not in the store modules — so the import graph stays acyclic:
 * wiring → stores → sdk-client → auth (auth imports none of these).
 */
export const resetAllWiring = async (): Promise<void> => {
  await sdkClient.hangupActive();
  resetSessionsWiring();
  resetCallsWiring();
  sdkClient.disconnect();
  queryClient.clear();
};
