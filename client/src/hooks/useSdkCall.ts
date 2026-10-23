import { useSyncExternalStore } from "react";
import type { OpenCall } from "wacalls-sdk";
import { sdkClient } from "@/lib/sdk-client";

/**
 * Bridges the SDK's imperative call handle to React: a version counter bumped
 * on every SDK call-lifecycle event, consumed via useSyncExternalStore so
 * components re-read `sdkClient.activeOpenCall` / `activeCallId` fresh.
 */
let version = 0;
const listeners = new Set<() => void>();

const bump = (): void => {
  version += 1;
  for (const l of listeners) l();
};

/** Notify React that the SDK's active call changed (e.g. after startCall/answerCall resolve). */
export const notifySdkCallChanged = bump;

let wired = false;
const ensureWired = (): void => {
  if (wired) return;
  wired = true;
  // Only call:ended bumps: it's the one SDK event that clears
  // activeOpenCall/activeCallId outside a mutation. call:status/broker:event
  // frames replay on SSE reconnect (one per row) and used to cause an O(N)
  // re-render storm; startCall/answerCall/hangup callers invoke
  // notifySdkCallChanged() explicitly after the SDK resolves.
  sdkClient.on("call:ended", bump);
};

const subscribe = (cb: () => void): (() => void) => {
  ensureWired();
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
  };
};

const getVersion = (): number => version;

export type SdkCallState = {
  activeCallId: string | null;
  conn: OpenCall | null;
};

/** Live view of the SDK-owned active call (re-renders on SDK call events). */
export const useSdkCallState = (): SdkCallState => {
  useSyncExternalStore(subscribe, getVersion, getVersion);
  return { activeCallId: sdkClient.activeCallId, conn: sdkClient.activeOpenCall };
};

/** The SDK-owned OpenCall for `callId`, or null when it isn't the active call. */
export const useSdkCallFor = (callId: string): OpenCall | null => {
  const { activeCallId, conn } = useSdkCallState();
  return activeCallId === callId ? conn : null;
};
