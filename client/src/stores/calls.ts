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
      // Identity is (sessionId, callId): two sessions can surface the same
      // WhatsApp call id, so a re-offer must not wipe the other session's row.
      const same = (c: { sessionId: string; callId: string }) =>
        c.sessionId === p.sessionId && c.callId === p.callId;
      // Dedupe by (sessionId, callId, offeredAt): a re-sent offer (SSE
      // reconnect replay) keeps the original timestamp AND the same queue
      // entry, so the ring loop isn't restarted by a duplicate frame.
      if (s.incoming.some((c) => same(c) && c.offeredAt === offeredAt)) return s;
      // Drop any stale entry for the same call (re-offer with new timestamp).
      const next = s.incoming.filter((c) => !same(c));
      next.push({ sessionId: p.sessionId, callId: p.callId, peer: p.peer, offeredAt });
      next.sort((a, b) => a.offeredAt - b.offeredAt);
      return { incoming: next };
    });
  });

  sdkClient.on("call:status", (p) => {
    useCalls.setState((s) => {
      const row = s.calls.find(
        (c) => c.callId === p.callId && c.sessionId === p.sessionId,
      );
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
          c.callId === p.callId && c.sessionId === p.sessionId
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
    const same = (c: { sessionId: string; callId: string }) =>
      c.callId === p.callId && c.sessionId === p.sessionId;
    const ended = useCalls.getState().calls.find(same);
    useCalls.setState((s) => {
      const hadCall = s.calls.some(same);
      const hadIncoming = s.incoming.some(same);
      if (!hadCall && !hadIncoming) return s;
      return {
        calls: hadCall ? s.calls.filter((c) => !same(c)) : s.calls,
        incoming: hadIncoming ? s.incoming.filter((c) => !same(c)) : s.incoming,
      };
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
      useCalls.setState((s) => {
        // The snapshot is already owner-scoped: a call claimed by another
        // operator drops out. Prune the incoming queue so a non-owner stops
        // ringing once someone else claims the call.
        const ids = new Set(ev.calls.map((c) => `${c.sessionId}\u0000${c.callId}`));
        const incoming = s.incoming.filter((c) => ids.has(`${c.sessionId}\u0000${c.callId}`));
        if (incoming.length === s.incoming.length) return { calls: ev.calls };
        return { calls: ev.calls, incoming };
      });
    } else if (ev.type === "incoming-claimed") {
      useCalls.setState((s) => {
        const hit = (c: { sessionId: string; callId: string }) =>
          c.callId === ev.id && c.sessionId === ev.sessionId;
        if (!s.incoming.some(hit)) return s;
        return { incoming: s.incoming.filter((c) => !hit(c)) };
      });
    }
  });
};

export const isMine = (call: CallSummary): boolean => call.owner === getClientId();

// removeIncoming drops one ringing entry. sessionId is part of the identity:
// two sessions may in theory surface the same WhatsApp call id, so the pair
// (sessionId, callId) is what identifies an entry.
export const removeIncoming = (sessionId: string, callId: string): void =>
  useCalls.setState((s) =>
    s.incoming.some((c) => c.sessionId === sessionId && c.callId === callId)
      ? { incoming: s.incoming.filter((c) => !(c.sessionId === sessionId && c.callId === callId)) }
      : s,
  );

export const clearIncoming = (): void => useCalls.setState({ incoming: [] });
