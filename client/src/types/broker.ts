import type { CallStatus } from "./call";
import type { SessionInfo, SessionState } from "./session";

type CallListRow = {
  sessionId: string;
  callId: string;
  owner: string | null;
  direction: "outbound" | "inbound";
  peer: string;
  startedAt: number;
  status: CallStatus;
  endedAt?: number;
  endReason?: string;
};

/**
 * Raw broker frames (SSE data payloads). The SDK re-emits every parsed frame
 * verbatim via `broker:event`; the app narrows it with this discriminated
 * union — the same shapes the Go broker broadcasts (cmd/server/broker.go).
 */
export type BrokerEvent =
  | { type: "session-list"; sessions: SessionInfo[] }
  | { type: "session-qr"; sessionId: string; qr: string }
  | { type: "auth-state"; sessionId: string; paired: boolean; state: SessionState; qr?: string }
  | { type: "call-list"; calls: CallListRow[] }
  | { type: "call-status"; sessionId: string; id: string; owner: string | null; status: CallStatus; peer: string; startedAt: number }
  | { type: "call-ended"; sessionId: string; id: string; owner: string | null; reason: string; endedAt: number }
  | { type: "incoming"; sessionId: string; id: string; peer: string; offeredAt: number }
  | { type: "incoming-claimed"; sessionId: string; id: string; owner: string };

/** Narrow the SDK's raw `broker:event` payload to the app's frame union. */
export const asBrokerEvent = (raw: Record<string, unknown>): BrokerEvent =>
  raw as unknown as BrokerEvent;
