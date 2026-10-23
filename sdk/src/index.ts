/**
 * ---------------------------------------------------------------------------
 * wacalls-sdk — vanilla TypeScript SDK for WaCalls
 * ---------------------------------------------------------------------------
 *
 * Zero runtime dependencies: only Web APIs (EventSource, fetch,
 * RTCPeerConnection, AudioContext/AudioWorklet, Blob/URL, DOM media).
 *
 * The facade owns:
 *  - SSE event stream ({baseUrl}/api/events) with data-frame JSON parsing,
 *    `: ping` comments ignored natively, exponential-backoff reconnect
 *    (1s → 30s cap) unless disconnect() was called; every parsed data frame
 *    is re-emitted verbatim as `broker:event` (raw frames such as
 *    session-list / auth-state / session-qr / call-list pass through there);
 *  - REST actions (start/accept/reject/hangup/webrtc signaling) via fetch,
 *    with X-Client-Id + optional Authorization: Bearer headers;
 *  - Call lifecycle: creates the RTCPeerConnection through the webrtc core
 *    (inside user-triggered startCall/answerCall, satisfying browser
 *    autoplay policy), renders the downlink on an internal <audio> element
 *    (with optional setSinkId output routing), and closes everything
 *    automatically on call-ended / hangup / disconnect.
 */

import { Emitter } from "./core/emitter";
import { openCall, type OpenCall, type Signaling } from "./core/webrtc";

export interface WaCallsConfig {
  /** API origin, e.g. "https://wa.example.com" — "" for same-origin. */
  baseUrl: string;
  /** Stable operator/client identifier (sent as X-Client-Id + SSE clientId). */
  clientId: string;
  /** Optional bearer token (REST) / query param (SSE — EventSource has no headers). */
  apiKey?: string;
  /** Optional exact microphone deviceId for all calls opened by this client. */
  micId?: string | null;
  /** Optional output deviceId (setSinkId) for the internal downlink <audio>. */
  outId?: string | null;
}

export type CallStatus = "starting" | "ringing" | "connected" | "ended";

const CALL_STATUSES: readonly string[] = ["starting", "ringing", "connected", "ended"];

/**
 * Type guard for the broker's status enum. Non-enum statuses are NOT cast
 * into typed `call:status` events — they stay reachable via `broker:event`.
 */
const isCallStatus = (v: unknown): v is CallStatus =>
  typeof v === "string" && CALL_STATUSES.includes(v);

export type IncomingCallPayload = {
  callId: string;
  peer: string;
  sessionId: string;
  /** Broker timestamp (epoch ms) of the offer, when present in the frame. */
  offeredAt?: number;
};
export type CallStatusPayload = {
  status: CallStatus;
  callId: string;
  sessionId: string;
  peer: string;
  owner: string | null;
  /** Broker timestamp (epoch ms) of call start, when present in the frame. */
  startedAt?: number;
};
export type CallEndedPayload = {
  reason: string;
  callId: string;
  sessionId: string;
  /** Broker timestamp (epoch ms) of the teardown, when present in the frame. */
  endedAt?: number;
};
export type SdkErrorPayload = { message: string };
/** Raw broker frame (SSE data JSON, always carries a `type` field). */
export type BrokerEventPayload = Record<string, unknown>;

/**
 * Structured error for every non-2xx REST response. `message` keeps the
 * legacy human-readable `"METHOD path status body"` shape, so existing UI
 * string-matching keeps working; use the fields for structured handling.
 */
export class SdkRequestError extends Error {
  readonly method: string;
  readonly path: string;
  readonly status: number;
  readonly body: string;

  constructor(method: string, path: string, status: number, body: string) {
    super(`${method} ${path} ${status}${body ? ` ${body}` : ""}`);
    this.name = "SdkRequestError";
    this.method = method;
    this.path = path;
    this.status = status;
    this.body = body;
  }
}

export interface WaCallsClientEvents {
  "call:incoming": IncomingCallPayload;
  "call:status": CallStatusPayload;
  "call:ended": CallEndedPayload;
  /** Passthrough of EVERY parsed SSE data frame (session-list, auth-state,
   *  session-qr, call-list, incoming-claimed, …) before the typed fan-out. */
  "broker:event": BrokerEventPayload;
  error: SdkErrorPayload;
}

export type StartCallOptions = {
  /** Max call duration in ms (reserved — currently ignored by the server). */
  durationMs?: number;
  /** Record the call server-side (reserved — currently ignored by the server). */
  record?: boolean;
};

type ActiveCall = {
  callId: string;
  sessionId: string;
  conn: OpenCall;
  audio: HTMLAudioElement | null;
};

type SinkCapableMedia = HTMLMediaElement & {
  setSinkId?: (sinkId: string) => Promise<void>;
};

const RECONNECT_BASE_MS = 1_000;
const RECONNECT_MAX_MS = 30_000;
const DEFAULT_DURATION_MS = 300_000;

const asString = (v: unknown): string => (typeof v === "string" ? v : "");

const asNumber = (v: unknown): number | undefined =>
  typeof v === "number" && Number.isFinite(v) ? v : undefined;

export class WaCallsClient {
  readonly #cfg: Required<Pick<WaCallsConfig, "baseUrl" | "clientId">> & WaCallsConfig;
  readonly #emitter = new Emitter<WaCallsClientEvents>();
  #es: EventSource | null = null;
  #reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  #reconnectMs = RECONNECT_BASE_MS;
  #stopped = true;
  #active: ActiveCall | null = null;
  /** Desired mute state — persisted across calls, applied to fresh mic tracks. */
  #muted = false;
  /** In-flight lock for #openMedia: only one media open at a time (first wins). */
  #mediaOpening = false;
  /** Generation token: bumped by each #openMedia claim and by disconnect() —
   *  a stale in-flight open closes its freshly created conn instead of
   *  orphaning it or overwriting `#active`. */
  #mediaGen = 0;

  constructor(config: WaCallsConfig) {
    if (typeof config?.clientId !== "string" || config.clientId === "") {
      throw new Error("WaCallsClient: config.clientId is required");
    }
    if (typeof config?.baseUrl !== "string") {
      throw new Error("WaCallsClient: config.baseUrl is required (use \"\" for same-origin)");
    }
    this.#cfg = { ...config, baseUrl: config.baseUrl.replace(/\/+$/, "") };
  }

  /* ---------------------------------------------------------------------- */
  /* Events                                                                  */
  /* ---------------------------------------------------------------------- */

  on<K extends keyof WaCallsClientEvents>(
    event: K,
    cb: (payload: WaCallsClientEvents[K]) => void,
  ): void {
    this.#emitter.on(event, cb);
  }

  off<K extends keyof WaCallsClientEvents>(
    event: K,
    cb: (payload: WaCallsClientEvents[K]) => void,
  ): void {
    this.#emitter.off(event, cb);
  }

  /* ---------------------------------------------------------------------- */
  /* SSE event stream                                                        */
  /* ---------------------------------------------------------------------- */

  /** Open the SSE stream (idempotent). Reconnects with backoff until disconnect(). */
  connect(): void {
    this.#stopped = false;
    if (this.#es) return;
    this.#reconnectMs = RECONNECT_BASE_MS;
    this.#openEventSource();
  }

  /** Close the SSE stream, cancel reconnects and release any active call. */
  disconnect(): void {
    this.#stopped = true;
    this.#es?.close();
    this.#es = null;
    if (this.#reconnectTimer !== null) {
      clearTimeout(this.#reconnectTimer);
      this.#reconnectTimer = null;
    }
    // Invalidate any in-flight #openMedia: the loser closes its own conn.
    this.#mediaGen += 1;
    const active = this.#active;
    if (active) {
      // Best-effort server-side end (fire-and-forget). Local teardown below
      // does not wait for it and runs even if the DELETE fails.
      void this.hangup(active.sessionId, active.callId).catch(() => {});
    }
    this.#teardownActive();
  }

  #openEventSource(): void {
    if (this.#stopped || this.#es) return;
    const params = new URLSearchParams({ clientId: this.#cfg.clientId });
    if (this.#cfg.apiKey) params.set("apiKey", this.#cfg.apiKey);
    const url = `${this.#cfg.baseUrl}/api/events?${params.toString()}`;

    let es: EventSource;
    try {
      es = new EventSource(url);
    } catch (err) {
      this.#emitError(`event stream open failed: ${String(err)}`);
      this.#scheduleReconnect();
      return;
    }
    this.#es = es;
    es.onopen = () => {
      this.#reconnectMs = RECONNECT_BASE_MS;
    };
    // Server sends only unnamed `data:` frames (+ `: ping` comments).
    // EventSource dispatches data frames via onmessage and ignores comments —
    // we additionally guard that each frame is a JSON object with a "type".
    es.onmessage = (ev) => this.#handleSseData(ev.data);
    es.onerror = () => {
      es.close();
      if (this.#es === es) this.#es = null;
      this.#scheduleReconnect();
    };
  }

  #scheduleReconnect(): void {
    if (this.#stopped || this.#reconnectTimer !== null) return;
    const delay = this.#reconnectMs;
    this.#reconnectMs = Math.min(this.#reconnectMs * 2, RECONNECT_MAX_MS);
    this.#reconnectTimer = setTimeout(() => {
      this.#reconnectTimer = null;
      this.#openEventSource();
    }, delay);
  }

  #handleSseData(data: string): void {
    let parsed: unknown;
    try {
      parsed = JSON.parse(data);
    } catch {
      this.#emitError("event stream: failed to parse data frame as JSON");
      return;
    }
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return;
    const ev = parsed as Record<string, unknown>;

    // Passthrough first: consumers that need raw frames (session-list,
    // auth-state, session-qr, call-list snapshots, incoming-claimed) get every
    // data frame here, BEFORE the typed fan-out below (so a call-list snapshot
    // lands before its per-row call:status expansions).
    this.#emitter.emit("broker:event", ev);

    switch (ev.type) {
      case "incoming": {
        const callId = asString(ev.id);
        const sessionId = asString(ev.sessionId);
        if (!callId || !sessionId) return;
        const offeredAt = asNumber(ev.offeredAt); // read from the raw frame
        this.#emitter.emit("call:incoming", {
          callId,
          sessionId,
          peer: asString(ev.peer),
          ...(offeredAt !== undefined ? { offeredAt } : {}),
        });
        return;
      }
      case "call-status": {
        // Guard the enum: non-enum statuses are NOT cast into typed events
        // (still available raw via broker:event above).
        const status = ev.status;
        if (!isCallStatus(status)) return;
        const startedAt = asNumber(ev.startedAt); // read from the raw frame
        this.#emitter.emit("call:status", {
          status,
          callId: asString(ev.id),
          sessionId: asString(ev.sessionId),
          peer: asString(ev.peer),
          owner: typeof ev.owner === "string" ? ev.owner : null,
          ...(startedAt !== undefined ? { startedAt } : {}),
        });
        return;
      }
      case "call-list": {
        // Snapshot replay on connect + bulk updates: one call:status per row.
        if (!Array.isArray(ev.calls)) return;
        for (const row of ev.calls) {
          if (typeof row !== "object" || row === null) continue;
          const r = row as Record<string, unknown>;
          const status = r.status;
          if (!isCallStatus(status)) continue;
          const startedAt = asNumber(r.startedAt); // per-row raw frame value
          this.#emitter.emit("call:status", {
            status,
            callId: asString(r.callId),
            sessionId: asString(r.sessionId),
            peer: asString(r.peer),
            owner: typeof r.owner === "string" ? r.owner : null,
            ...(startedAt !== undefined ? { startedAt } : {}),
          });
        }
        return;
      }
      case "call-ended": {
        const callId = asString(ev.id);
        const endedAt = asNumber(ev.endedAt); // read from the raw frame
        const payload: CallEndedPayload = {
          reason: asString(ev.reason),
          callId,
          sessionId: asString(ev.sessionId),
          ...(endedAt !== undefined ? { endedAt } : {}),
        };
        this.#emitter.emit("call:ended", payload);
        if (this.#active && this.#active.callId === callId) this.#teardownActive();
        return;
      }
      default:
        // session-list / session-qr / auth-state / incoming-claimed (and any
        // frame whose status is outside the enum): not mapped to typed events
        // — available via the broker:event passthrough.
        return;
    }
  }

  /* ---------------------------------------------------------------------- */
  /* REST actions                                                            */
  /* ---------------------------------------------------------------------- */

  /** POST /api/sessions/{sid}/calls — then opens media (user-triggered). */
  async startCall(
    sessionId: string,
    phone: string,
    options: StartCallOptions = {},
  ): Promise<void> {
    const res = await this.#request<{ call?: { callId?: string } }>(
      "POST",
      `/api/sessions/${encodeURIComponent(sessionId)}/calls`,
      {
        phone,
        duration_ms: options.durationMs ?? DEFAULT_DURATION_MS,
        record: options.record ?? false,
      },
    );
    const callId = res?.call?.callId;
    if (!callId) {
      const msg = "startCall: response missing call.callId";
      this.#emitError(msg);
      throw new Error(msg);
    }
    await this.#openMedia(sessionId, callId);
  }

  /** POST .../accept — then opens media (user-triggered; autoplay-safe). */
  async answerCall(sessionId: string, callId: string): Promise<void> {
    const res = await this.#request<{ call?: { callId?: string } }>(
      "POST",
      `/api/sessions/${encodeURIComponent(sessionId)}/calls/${encodeURIComponent(callId)}/accept`,
      {},
    );
    // Prefer the server-returned callId (fallback to the argument) when
    // opening media — matches the pre-SDK React client.
    const serverCallId = res?.call?.callId || callId;
    await this.#openMedia(sessionId, serverCallId);
  }

  /** POST .../reject — no media involved. */
  async rejectCall(sessionId: string, callId: string): Promise<void> {
    await this.#request("POST", `/api/sessions/${encodeURIComponent(sessionId)}/calls/${encodeURIComponent(callId)}/reject`, {});
  }

  /** DELETE .../calls/{callId} — then closes local media for that call. */
  async hangup(sessionId: string, callId: string): Promise<void> {
    const active = this.#active;
    // A stale sessionId with a matching active callId must still address the
    // live call — the DELETE uses the active call's sessionId in that case.
    const sid =
      active && active.callId === callId && active.sessionId !== sessionId
        ? active.sessionId
        : sessionId;
    try {
      await this.#request("DELETE", `/api/sessions/${encodeURIComponent(sid)}/calls/${encodeURIComponent(callId)}`);
    } finally {
      // Local teardown must not depend on DELETE success.
      if (this.#active && this.#active.callId === callId) this.#teardownActive();
    }
  }

  /** Mute/unmute the mic (track.enabled negation); stores the desired state
   *  and applies it to freshly opened mics as well. */
  toggleMute(isMuted: boolean): void {
    this.#muted = isMuted;
    const tracks = this.#active?.conn.micStream.getAudioTracks() ?? [];
    for (const t of tracks) t.enabled = !isMuted;
  }

  /** Desired mute state (survives call changes; see toggleMute). */
  get isMuted(): boolean {
    return this.#muted;
  }

  /** Change the mic used for subsequently opened calls (null = default). */
  setMicId(micId: string | null): void {
    this.#cfg.micId = micId;
  }

  /** Change the output device; immediately re-routes the active downlink.
   *  null resets the sink to the system default (setSinkId("")). */
  setOutId(outId: string | null): void {
    this.#cfg.outId = outId;
    const audio = this.#active?.audio;
    if (audio) void this.#applySink(audio, outId ?? "");
  }

  /** callId of the locally-open call, if any. */
  get activeCallId(): string | null {
    return this.#active?.callId ?? null;
  }

  /** The raw OpenCall of the active call (advanced use: monitors, stats). */
  get activeOpenCall(): OpenCall | null {
    return this.#active?.conn ?? null;
  }

  /** The internal downlink <audio> of the active call (advanced use: mute it
   *  while an external monitor renders the same stream — single-renderer rule). */
  get activeAudio(): HTMLAudioElement | null {
    return this.#active?.audio ?? null;
  }

  /* ---------------------------------------------------------------------- */
  /* Media lifecycle (internal)                                              */
  /* ---------------------------------------------------------------------- */

  async #openMedia(sessionId: string, callId: string): Promise<void> {
    // One active call at a time — replacing a different call tears it down.
    if (this.#active) {
      if (this.#active.callId === callId) return;
      this.#teardownActive();
    }
    // In-flight lock + generation token: two overlapping startCall/answerCall
    // must not both open media and overwrite `#active`. First wins (holds the
    // lock through its await); the second rejects BEFORE touching the mic, so
    // there is no orphaned connection either way. The generation token also
    // catches the loser of a disconnect() racing an open in progress: it
    // closes its freshly created conn and rethrows instead of assigning
    // `#active`.
    if (this.#mediaOpening) {
      throw new Error("openMedia: another call setup is already opening media (first wins, second rejects)");
    }
    this.#mediaOpening = true;
    const gen = ++this.#mediaGen; // claim this attempt
    try {
      const signaling: Signaling = {
        sendOffer: async (sid, cid, sdpOffer) => {
          const res = await this.#request<{ sdp_answer?: string }>(
            "POST",
            `/api/sessions/${encodeURIComponent(sid)}/calls/${encodeURIComponent(cid)}/webrtc`,
            { sdp_offer: sdpOffer },
          );
          if (!res?.sdp_answer) throw new Error("webrtc signaling: response missing sdp_answer");
          return res.sdp_answer;
        },
      };
      const conn = await openCall({ sessionId, callId, signaling, micId: this.#cfg.micId ?? null });
      if (gen !== this.#mediaGen) {
        // Superseded while opening (e.g. disconnect()): close the freshly
        // created conn — never leave it orphaned and never overwrite #active.
        try {
          conn.close();
        } catch {}
        throw new Error("openMedia: call setup superseded (client disconnected)");
      }
      // Apply the stored mute preference to the fresh mic tracks.
      if (this.#muted) {
        for (const t of conn.micStream.getAudioTracks()) t.enabled = false;
      }
      const audio = this.#renderRemote(conn, this.#cfg.outId ?? null);
      this.#active = { callId, sessionId, conn, audio };
    } finally {
      this.#mediaOpening = false;
    }
  }

  #renderRemote(conn: OpenCall, outId: string | null): HTMLAudioElement | null {
    if (typeof document === "undefined" || !conn.remoteStream) return null;
    const el = document.createElement("audio");
    el.autoplay = true;
    el.srcObject = conn.remoteStream;
    if (outId) void this.#applySink(el, outId);
    el.play().catch((err) => {
      this.#emitError(`downlink playback failed: ${String(err)}`);
    });
    return el;
  }

  async #applySink(el: HTMLMediaElement, sinkId: string): Promise<void> {
    const sinkEl = el as SinkCapableMedia;
    if (typeof sinkEl.setSinkId !== "function") return;
    try {
      await sinkEl.setSinkId(sinkId);
    } catch (err) {
      this.#emitError(`setSinkId failed: ${String(err)}`);
    }
  }

  #teardownActive(): void {
    const active = this.#active;
    if (!active) return;
    this.#active = null;
    try {
      active.conn.close(); // stops mic tracks + closes AudioContext + pc
    } catch {}
    if (active.audio) {
      try {
        active.audio.pause();
      } catch {}
      try {
        active.audio.srcObject = null;
      } catch {}
      try {
        active.audio.remove();
      } catch {}
    }
  }

  /* ---------------------------------------------------------------------- */
  /* HTTP (internal)                                                         */
  /* ---------------------------------------------------------------------- */

  #headers(): Record<string, string> {
    const h: Record<string, string> = {
      "Content-Type": "application/json",
      "X-Client-Id": this.#cfg.clientId,
    };
    if (this.#cfg.apiKey) h["Authorization"] = `Bearer ${this.#cfg.apiKey}`;
    return h;
  }

  async #request<T>(method: "POST" | "DELETE", path: string, body?: unknown): Promise<T> {
    const url = `${this.#cfg.baseUrl}${path}`;
    const res = await fetch(url, {
      method,
      headers: this.#headers(),
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      // Structured error for every non-2xx REST response. The `message`
      // keeps the legacy "METHOD path status body" shape for UI matching.
      throw new SdkRequestError(method, path, res.status, text);
    }
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    if (!text) return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      return undefined as T;
    }
  }

  #emitError(message: string): void {
    this.#emitter.emit("error", { message });
  }
}

/* -------------------------------------------------------------------------- */
/* Re-exports — low-level engine for advanced embedders                        */
/* -------------------------------------------------------------------------- */

export { openCall } from "./core/webrtc";
export {
  isDownlinkMonitorActive,
  setDownlinkMonitorSink,
  startDownlinkMonitor,
  stopDownlinkMonitor,
  PLC_ENABLED,
  PCM_CHANNEL_LABEL,
  SAMPLE_RATE,
  type OpenCall,
  type OpenCallOptions,
  type Signaling,
} from "./core/webrtc";
export { float32ToInt16LE, int16LEToFloat32 } from "./utils/pcm";
export { Emitter } from "./core/emitter";
