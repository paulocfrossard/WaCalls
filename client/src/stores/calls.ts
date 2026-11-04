import { create } from "zustand";
import { sdkClient } from "@/lib/sdk-client";
import { getClientId } from "@/lib/client-id";
import { queryClient, queryKeys } from "@/lib/query";
import { asBrokerEvent } from "@/types/broker";
import type { CallSummary, IncomingPayload } from "@/types/call";
import type { HistoryRow } from "@/types/history";

type State = {
  calls: CallSummary[];
  incoming: IncomingPayload[];
};

export const useCalls = create<State>(() => ({
  calls: [],
  incoming: [],
}));

let wired = false;
export const ensureCallsWired = (): void => {
  if (wired) return;
  wired = true;

  // Typed SDK call events (SDK owns the SSE stream; call-list snapshots are
  // fanned out to per-row call:status events by the SDK as well).
  sdkClient.on("call:incoming", (p) => {
    useCalls.setState((s) => {
      const offeredAt = p.offeredAt ?? Date.now();
      // Dedupe by (callId, offeredAt): a re-sent offer (SSE reconnect replay)
      // keeps the original timestamp AND the same queue entry, so the ring
      // loop isn't restarted by a duplicate frame.
      if (s.incoming.some((c) => c.callId === p.callId && c.offeredAt === offeredAt)) return s;
      // Drop any stale entry for the same callId (re-offer with new timestamp).
      const next = s.incoming.filter((c) => c.callId !== p.callId);
      next.push({ sessionId: p.sessionId, callId: p.callId, peer: p.peer, offeredAt });
      next.sort((a, b) => a.offeredAt - b.offeredAt);
      return { incoming: next };
    });
  });

  sdkClient.on("call:status", (p) => {
    useCalls.setState((s) => {
      const row = s.calls.find((c) => c.callId === p.callId);
      // Short-circuit on no-op updates (SSE snapshot replay): returning the
      // same state object makes zustand skip the notify (no re-render).
      // `!row` too: a status frame for an unknown call changes nothing (the
      // map below only ever updates existing rows).
      if (
        !row ||
        (row.status === p.status &&
          row.sessionId === p.sessionId &&
          row.peer === p.peer &&
          row.startedAt === (p.startedAt ?? row.startedAt))
      ) {
        return s;
      }
      return {
        calls: s.calls.map((c) =>
          c.callId === p.callId
            ? {
                ...c,
                sessionId: p.sessionId,
                status: p.status,
                peer: p.peer,
                // Merge startedAt from the payload when present (previously
                // dropped → frozen duration UI on connected rows).
                startedAt: p.startedAt ?? c.startedAt,
              }
            : c,
        ),
      };
    });
  });

  sdkClient.on("call:ended", (p) => {
    // endedAt from the broker frame when present — used to seed the history
    // cache optimistically so the drawer reflects the ended call immediately
    // (the server appends to history BEFORE broadcasting call-ended, so the
    // invalidation below reconciles with the authoritative row).
    const endedAt = p.endedAt ?? Date.now();
    const ended = useCalls.getState().calls.find((c) => c.callId === p.callId);
    useCalls.setState((s) => {
      const calls = s.calls.filter((c) => c.callId !== p.callId);
      const incoming = s.incoming.some((c) => c.callId === p.callId)
        ? s.incoming.filter((c) => c.callId !== p.callId)
        : s.incoming;
      if (calls === s.calls && incoming === s.incoming) return s;
      return { calls, incoming };
    });
    if (ended) {
      queryClient.setQueryData<HistoryRow[]>(["history", p.sessionId], (rows) => {
        if (!rows || rows.some((r) => r.callId === p.callId)) return rows;
        return [
          {
            callId: ended.callId,
            peer: ended.peer,
            direction: ended.direction,
            startedAt: ended.startedAt,
            endedAt,
            endReason: p.reason || null,
          },
          ...rows,
        ];
      });
    }
    void queryClient.invalidateQueries({ queryKey: queryKeys.history });
  });

  // Raw broker passthrough for frames the SDK doesn't type: the full
  // call-list snapshot (adds/refreshes rows with startedAt/direction) and
  // incoming-claimed. broker:event fires BEFORE the typed fan-out, so the
  // snapshot lands first and per-row call:status updates after.
  sdkClient.on("broker:event", (raw) => {
    const ev = asBrokerEvent(raw);
    if (ev.type === "call-list") {
      useCalls.setState({ calls: ev.calls });
    } else if (ev.type === "incoming-claimed") {
      useCalls.setState((s) => {
        if (!s.incoming.some((c) => c.callId === ev.id)) return s;
        return { incoming: s.incoming.filter((c) => c.callId !== ev.id) };
      });
    }
  });
};

export const isMine = (call: CallSummary): boolean => call.owner === getClientId();

export const removeIncoming = (callId: string): void =>
  useCalls.setState((s) =>
    s.incoming.some((c) => c.callId === callId)
      ? { incoming: s.incoming.filter((c) => c.callId !== callId) }
      : s,
  );

export const clearIncoming = (): void => useCalls.setState({ incoming: [] });
