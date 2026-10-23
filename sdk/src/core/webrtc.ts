/**
 * ---------------------------------------------------------------------------
 * WebRTC + PCM DataChannel engine — port of client/src/lib/webrtc.ts
 * ---------------------------------------------------------------------------
 *
 * Differences from the app-internal engine (intentional):
 *  - No REST/fetch of its own: SDP exchange goes through the injected
 *    `signaling` interface (`sendOffer` returns the sdp_answer).
 *  - AudioWorklet processors are installed from embedded source strings
 *    (see ../worklets/sources.ts) via Blob URLs — no `/worklets/...` fetches.
 *  - No app-coupled debug ring; optional audio flags are plain options
 *    instead of `window.location.search` URL switches.
 */

import { float32ToInt16LE, int16LEToFloat32 } from "../utils/pcm";
import { addWorkletModule } from "../utils/worklet-loader";
import {
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_PROCESSOR_SRC,
  PLAYBACK_PROCESSOR_NAME,
  PLAYBACK_PROCESSOR_SRC,
} from "../worklets/sources";

export const SAMPLE_RATE = 16000;
export const PCM_CHANNEL_LABEL = "pcm";
export const PLC_ENABLED = true;
/** Max time to wait for ICE gathering before sending the offer anyway. */
const ICE_GATHERING_TIMEOUT_MS = 3_000;
/** Uplink backpressure: drop frames once the DataChannel queued this much. */
const DC_BUFFERED_AMOUNT_LIMIT = 65_536;

/** Minimal signaling contract — implemented by the SDK facade (or the embedder). */
export type Signaling = {
  /** Send the local SDP offer, resolve with the remote SDP answer. */
  sendOffer: (sessionId: string, callId: string, sdpOffer: string) => Promise<string>;
};

export type OpenCallOptions = {
  sessionId: string;
  callId: string;
  signaling: Signaling;
  /** Optional exact microphone deviceId. */
  micId?: string | null;
  echoCancellation?: boolean;
  noiseSuppression?: boolean;
  autoGainControl?: boolean;
  plcEnabled?: boolean;
};

export type OpenCall = {
  pc: RTCPeerConnection;
  micStream: MediaStream;
  remoteStream: MediaStream | null;
  ctx: AudioContext;
  close: () => void;
};

export const openCall = async (opts: OpenCallOptions): Promise<OpenCall> => {
  const { sessionId, callId, signaling, micId = null } = opts;
  const echoCancellation = opts.echoCancellation ?? true;
  const noiseSuppression = opts.noiseSuppression ?? true;
  const autoGainControl = opts.autoGainControl ?? true;
  const plcEnabled = opts.plcEnabled ?? PLC_ENABLED;

  const audioConstraints: MediaTrackConstraints = {
    ...(micId ? { deviceId: { exact: micId } } : {}),
    echoCancellation,
    noiseSuppression,
    autoGainControl,
  };
  const micStream = await navigator.mediaDevices.getUserMedia({ audio: audioConstraints });

  const pc = new RTCPeerConnection({ iceServers: [] });

  const dc = pc.createDataChannel(PCM_CHANNEL_LABEL, { ordered: true });
  dc.binaryType = "arraybuffer";

  let ctx: AudioContext | null = null;
  let remoteStream: MediaStream | null = null;

  // HOISTED CLEANUP: runs for ANY failure after the mic is live —
  // AudioContext construction, worklet install, createOffer,
  // setLocalDescription, ICE wait, signaling.sendOffer, setRemoteDescription.
  // No path may leave a live mic with no retained handle.
  const cleanup = (): void => {
    try {
      micStream.getTracks().forEach((t) => t.stop());
    } catch {}
    if (ctx) {
      try {
        ctx.close();
      } catch {}
    }
    try {
      pc.close();
    } catch {}
  };

  try {
    ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
    await addWorkletModule(ctx, CAPTURE_PROCESSOR_SRC);
    await addWorkletModule(ctx, PLAYBACK_PROCESSOR_SRC);
    await ctx.resume();

    const micSource = ctx.createMediaStreamSource(micStream);
    const captureNode = new AudioWorkletNode(ctx, CAPTURE_PROCESSOR_NAME);
    captureNode.port.onmessage = (e: MessageEvent<Float32Array>) => {
      if (dc.readyState !== "open") return;
      // Audio freshness beats completeness: when the channel is congested,
      // drop the frame instead of queueing stale audio behind it.
      if (dc.bufferedAmount > DC_BUFFERED_AMOUNT_LIMIT) return;
      dc.send(float32ToInt16LE(e.data));
    };
    micSource.connect(captureNode);
    captureNode.connect(ctx.destination);

    const playbackNode = new AudioWorkletNode(ctx, PLAYBACK_PROCESSOR_NAME, {
      processorOptions: { plcEnabled },
    });
    const streamDest = ctx.createMediaStreamDestination();
    playbackNode.connect(streamDest);
    remoteStream = streamDest.stream;
    dc.onmessage = (e: MessageEvent<ArrayBuffer>) => {
      const samples = int16LEToFloat32(e.data);
      // Transfer (zero-copy) instead of structured clone — the buffer is
      // freshly allocated per frame, so nothing else references it.
      playbackNode.port.postMessage(samples, [samples.buffer]);
    };

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    // ICE gathering with a ~3s budget: resolve on completion OR timeout,
    // then proceed with whatever localDescription exists (never hangs forever
    // on a peer that never reports "complete").
    await new Promise<void>((resolve) => {
      if (pc.iceGatheringState === "complete") {
        resolve();
        return;
      }
      const onGatheringState = (): void => {
        if (pc.iceGatheringState !== "complete") return;
        clearTimeout(timer);
        pc.removeEventListener("icegatheringstatechange", onGatheringState);
        resolve();
      };
      const timer = setTimeout(() => {
        pc.removeEventListener("icegatheringstatechange", onGatheringState);
        resolve();
      }, ICE_GATHERING_TIMEOUT_MS);
      pc.addEventListener("icegatheringstatechange", onGatheringState);
    });

    const sdpAnswer = await signaling.sendOffer(sessionId, callId, pc.localDescription!.sdp);
    await pc.setRemoteDescription({ type: "answer", sdp: sdpAnswer });
  } catch (err) {
    cleanup();
    throw err;
  }

  return {
    pc,
    micStream,
    remoteStream,
    ctx: ctx as AudioContext, // non-null: the try block above completed
    close: cleanup, // same closure — stops tracks + closes AudioContext + pc
  };
};

/**
 * ---------------------------------------------------------------------------
 * Downlink monitor — "escutar o que o cliente escuta"
 * ---------------------------------------------------------------------------
 *
 * Renders `conn.remoteStream` (DOWNLINK only) through a dedicated
 * <audio> element:
 *   dc.onmessage -> playbackNode.port.postMessage -> playback worklet
 *   -> AudioWorkletNode -> MediaStreamDestination -> remoteStream
 *
 * ANTI-LOOP GUARANTEES (never violated by this module):
 *
 * 1. No software feedback loop: the monitor API only accepts an `OpenCall`
 *    and internally reads `conn.remoteStream`. The uplink
 *    (`micStream` -> capture worklet -> dc.send) can NOT be wired to the
 *    monitor: there is no code path that passes a MediaStream in, and a
 *    defensive check rejects the case where remoteStream === micStream
 *    (monitoring your own mic would be an instant howl/feedback).
 *
 * 2. Single renderer rule: while the monitor is ON the primary <audio> is
 *    muted (its srcObject/stream is untouched and is restored on stop).
 *    Rendering the same downlink twice into the same output device would
 *    play it twice with different latencies => comb-filter/"echo" artefacts.
 *    One active renderer per output, always.
 *
 * 3. Acoustic loop (browser can't fix it): if the monitored downlink leaves
 *    through a SPEAKER that the MIC picks up, you get acoustic echo/feedback
 *    — the browser's AEC only cancels what it renders through the local
 *    playback path it knows about, and the monitor is an extra path.
 *    => Always use HEADPHONES while monitoring.
 *
 * Only ONE monitor exists at a time (module singleton, `owner` = callId) so
 * two call cards can never stack two renderings of the same downlink.
 */

type MonitorState = {
  owner: string;
  el: HTMLAudioElement;
  primary: HTMLAudioElement | null;
  primaryWasMuted: boolean;
};

/** `setSinkId` is not in every TS DOM lib / browser — probe before use. */
type SinkCapableMedia = HTMLMediaElement & {
  setSinkId?: (sinkId: string) => Promise<void>;
};

let monitor: MonitorState | null = null;

const applySinkId = async (
  el: HTMLMediaElement,
  sinkId: string | null,
  what: string,
): Promise<void> => {
  if (!sinkId) return;
  const sinkEl = el as SinkCapableMedia;
  if (typeof sinkEl.setSinkId !== "function") {
    console.warn(`[monitor] setSinkId unsupported in this browser (${what}); keeping default output.`);
    return;
  }
  try {
    await sinkEl.setSinkId(sinkId);
  } catch (err) {
    console.warn(`[monitor] setSinkId failed (${what}); keeping default output.`, err);
  }
};

/** True while a downlink monitor is playing. Pass `owner` (callId) to scope it. */
export const isDownlinkMonitorActive = (owner?: string): boolean =>
  monitor !== null && (owner === undefined || monitor.owner === owner);

/**
 * Start listening to the DOWNLINK (what the customer receives from us, plus
 * PLC/jitter as it leaves this browser).
 *
 * @param owner callId that owns this monitor (only one monitor at a time).
 * @param conn  the open call; only `remoteStream` (downlink) is ever used.
 * @param opts.sinkId  output device (same outDeviceId as the primary audio).
 * @param opts.primary primary <audio> element, muted while the monitor plays.
 * @returns true if the monitor is actually playing.
 */
export const startDownlinkMonitor = async (
  owner: string,
  conn: Pick<OpenCall, "micStream" | "remoteStream">,
  opts: { sinkId?: string | null; primary?: HTMLAudioElement | null } = {},
): Promise<boolean> => {
  const stream = conn.remoteStream;
  if (!stream) {
    console.warn("[monitor] no remoteStream yet; monitor stays off.");
    return false;
  }
  // Hard anti-loop: never monitor the uplink (own mic).
  if (stream === conn.micStream) {
    console.error("[monitor] refused: remoteStream is the mic stream (uplink) — that would feed back.");
    return false;
  }
  if (typeof document === "undefined") return false;

  stopDownlinkMonitor(); // one monitor at a time (takes over from any owner)

  const el = document.createElement("audio");
  el.autoplay = true;
  el.muted = false;
  el.srcObject = stream;
  el.addEventListener("error", () => {
    if (monitor?.el === el) stopDownlinkMonitor(owner);
  });

  await applySinkId(el, opts.sinkId ?? null, "monitor");
  try {
    await el.play();
  } catch (err) {
    console.warn("[monitor] unable to start the downlink monitor", err);
    el.srcObject = null;
    return false;
  }

  const primary = opts.primary ?? null;
  const primaryWasMuted = primary?.muted ?? false;
  if (primary && !primaryWasMuted) primary.muted = true; // single renderer rule

  monitor = { owner, el, primary, primaryWasMuted };
  console.log("[monitor] downlink monitor ON (headphones recommended)");
  return true;
};

/** Stop the monitor. With `owner`, only stops it if that owner started it. */
export const stopDownlinkMonitor = (owner?: string): void => {
  const active = monitor;
  if (!active) return;
  if (owner !== undefined && active.owner !== owner) return;
  monitor = null;
  const { el, primary, primaryWasMuted } = active;
  try {
    el.pause();
  } catch {}
  try {
    el.srcObject = null; // OFF => release the stream/resources
  } catch {}
  if (primary) primary.muted = primaryWasMuted; // hand playback back to the primary <audio>
  console.log("[monitor] downlink monitor OFF");
};

/** Re-route the active monitor (e.g. when the user changes the output device). */
export const setDownlinkMonitorSink = async (sinkId: string | null): Promise<void> => {
  if (!monitor) return;
  await applySinkId(monitor.el, sinkId, "monitor");
};
