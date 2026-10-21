import { apiPost } from "./api";
import { debugLog, debugLogRate, errFields } from "./debug-log";
import { float32ToInt16LE, int16LEToFloat32 } from "./pcm";
import {
  AEC_ENABLED,
  AGC_ENABLED,
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_WORKLET_URL,
  NS_ENABLED,
  PCM_CHANNEL_LABEL,
  PLC_ENABLED,
  PLAYBACK_PROCESSOR_NAME,
  PLAYBACK_WORKLET_URL,
  SAMPLE_RATE,
} from "../constants/audio";

export type OpenCall = {
  pc: RTCPeerConnection;
  micStream: MediaStream;
  remoteStream: MediaStream | null;
  ctx: AudioContext;
  close: () => void;
};

export const openCall = async (
  sid: string,
  callId: string,
  micDeviceId: string | null,
): Promise<OpenCall> => {
  const audioConstraints = {
    ...(micDeviceId ? { deviceId: { exact: micDeviceId } } : {}),
    // field A/B: append ?aec=0 / ?ns=0 / ?agc=0 to disable echoCancellation /
    // noiseSuppression / autoGainControl for one call (diagnosis only).
    echoCancellation: AEC_ENABLED,
    noiseSuppression: NS_ENABLED,
    autoGainControl: AGC_ENABLED,
  };
  let micStream: MediaStream;
  try {
    micStream = await navigator.mediaDevices.getUserMedia({ audio: audioConstraints });
  } catch (err) {
    debugLog("gum", "error", "getUserMedia failed", {
      callId,
      deviceId: micDeviceId ?? "default",
      constraints: audioConstraints,
      ...errFields(err),
    });
    throw err;
  }
  const micTrack = micStream.getAudioTracks()[0];
  debugLog("gum", "info", "getUserMedia ok", {
    callId,
    deviceId: micDeviceId ?? "default",
    constraints: audioConstraints,
    track: micTrack ? { label: micTrack.label, settings: micTrack.getSettings() } : null,
  });

  const pc = new RTCPeerConnection({ iceServers: [] });

  const dc = pc.createDataChannel(PCM_CHANNEL_LABEL, { ordered: true });
  dc.binaryType = "arraybuffer";

  // Lifecycle + fatal channel errors — the "is the uplink pipe alive?" trail.
  dc.onopen = () => {
    debugLog("dc", "info", "datachannel open", {
      callId,
      label: dc.label,
      bufferedAmount: dc.bufferedAmount,
    });
  };
  dc.onclose = () => {
    debugLog("dc", "info", "datachannel close", { callId, label: dc.label, readyState: dc.readyState });
  };
  dc.onerror = (ev: Event) => {
    const rtcErr = (ev as RTCErrorEvent).error;
    debugLog("dc", "error", "datachannel error", {
      callId,
      name: rtcErr?.name ?? "error",
      message: rtcErr?.message ?? "",
    });
  };

  let ctx: AudioContext;
  try {
    ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
  } catch (err) {
    debugLog("audio", "error", "AudioContext failed", { callId, sampleRate: SAMPLE_RATE, ...errFields(err) });
    throw err;
  }
  try {
    await ctx.audioWorklet.addModule(CAPTURE_WORKLET_URL);
    await ctx.audioWorklet.addModule(PLAYBACK_WORKLET_URL);
    await ctx.resume();
  } catch (err) {
    debugLog("audio", "error", "audio worklet module/resume failed", {
      callId,
      sampleRate: SAMPLE_RATE,
      state: ctx.state,
      ...errFields(err),
    });
    throw err;
  }

  // Uplink backlog: warn once when the send buffer crosses 1 MB, re-arm below.
  const BACKLOG_WARN_BYTES = 1024 * 1024;
  let backlogWarned = false;

  const micSource = ctx.createMediaStreamSource(micStream);
  const captureNode = new AudioWorkletNode(ctx, CAPTURE_PROCESSOR_NAME);
  captureNode.port.onmessage = (e: MessageEvent<Float32Array>) => {
    if (dc.readyState !== "open") return;
    // Browser→transport boundary: peak/rms of the exact Float32Array that
    // becomes uplink (post-gUM/AEC/NS/AGC) — field evidence for "is the mic
    // really silent when the user is quiet?". Values from the frame that
    // triggers the rate-limited emission.
    const data = e.data;
    let peak = 0;
    let sumSquares = 0;
    for (let i = 0; i < data.length; i += 1) {
      const v = data[i];
      const a = v < 0 ? -v : v;
      if (a > peak) peak = a;
      sumSquares += v * v;
    }
    const rms = data.length > 0 ? Math.sqrt(sumSquares / data.length) : 0;
    const frame = float32ToInt16LE(data);
    dc.send(frame);
    debugLogRate({
      key: `dc-send:${callId}`,
      source: "uplink",
      level: "debug",
      msg: "dc.send → relay (what the mic sends)",
      bytes: frame.byteLength,
      intervalMs: 500, // rate-limit 2/s
      extra: { rms, peak },
    });
    if (dc.bufferedAmount > BACKLOG_WARN_BYTES) {
      if (!backlogWarned) {
        backlogWarned = true;
        debugLog("dc", "warn", "dc.bufferedAmount > 1 MB (uplink backlog)", {
          callId,
          bufferedAmount: dc.bufferedAmount,
          readyState: dc.readyState,
        });
      }
    } else if (backlogWarned) {
      backlogWarned = false;
      debugLog("dc", "info", "dc.bufferedAmount back below 1 MB", {
        callId,
        bufferedAmount: dc.bufferedAmount,
      });
    }
  };
  micSource.connect(captureNode);
  captureNode.connect(ctx.destination);

  const plcParam = new URLSearchParams(window.location.search).get("plc");
  const plcEnabled = plcParam !== null ? plcParam !== "0" : PLC_ENABLED;
  const playbackNode = new AudioWorkletNode(ctx, PLAYBACK_PROCESSOR_NAME, {
    processorOptions: { plcEnabled },
  });
  const streamDest = ctx.createMediaStreamDestination();
  playbackNode.connect(streamDest);
  playbackNode.port.onmessage = (e: MessageEvent) => {
    if (e.data?.type === "stats") {
      console.log("[plc-stats]", e.data);
      debugLog("plc", "debug", "[plc-stats]", e.data as Record<string, unknown>);
    }
  };
  dc.onmessage = (e: MessageEvent<ArrayBuffer>) => {
    playbackNode.port.postMessage(int16LEToFloat32(e.data));
    debugLogRate({
      key: `dc-recv:${callId}`,
      source: "downlink",
      level: "debug",
      msg: "dc.onmessage ← relay (what the client hears)",
      bytes: e.data.byteLength,
      intervalMs: 500, // rate-limit 2/s
    });
  };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await new Promise<void>((resolve) => {
    if (pc.iceGatheringState === "complete") resolve();
    else
      pc.addEventListener("icegatheringstatechange", () => {
        if (pc.iceGatheringState === "complete") resolve();
      });
  });

  const { sdp_answer } = await apiPost<{ sdp_answer: string }>(
    `/api/sessions/${sid}/calls/${callId}/webrtc`,
    { sdp_offer: pc.localDescription!.sdp },
  );
  await pc.setRemoteDescription({ type: "answer", sdp: sdp_answer });

  return {
    pc,
    micStream,
    remoteStream: streamDest.stream,
    ctx, // exposed read-only for field instrumentation (audioCtx.state sampler)
    close: () => {
      try {
        micStream.getTracks().forEach((t) => t.stop());
      } catch {}
      try {
        ctx.close();
      } catch {}
      try {
        pc.close();
      } catch {}
    },
  };
};

/**
 * ---------------------------------------------------------------------------
 * Downlink monitor — "escutar o que o cliente escuta"
 * ---------------------------------------------------------------------------
 *
 * The monitor renders `conn.remoteStream` (DOWNLINK only):
 *   dc.onmessage -> playbackNode.port.postMessage -> playback worklet
 *   -> AudioWorkletNode -> MediaStreamDestination -> remoteStream
 * i.e. the exact signal that leaves the card through the primary <audio>.
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
 *    => Always use HEADPHONES while monitoring. The UI exposes this hint
 *    (tooltip + inline hint) next to the toggle.
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

let monitor: MonitorState | null = null;

const applySinkId = async (
  el: HTMLMediaElement,
  sinkId: string | null,
  what: string,
): Promise<void> => {
  if (!sinkId) return;
  if (typeof el.setSinkId !== "function") {
    console.warn(`[monitor] setSinkId unsupported in this browser (${what}); keeping default output.`);
    debugLog("sink", "warn", "setSinkId unsupported in this browser", { what, sinkId });
    return;
  }
  try {
    await el.setSinkId(sinkId);
    debugLog("sink", "info", "setSinkId applied", { what, sinkId });
  } catch (err) {
    console.warn(`[monitor] setSinkId failed (${what}); keeping default output.`, err);
    debugLog("sink", "warn", "setSinkId failed", { what, sinkId, ...errFields(err) });
  }
};

/** True while a downlink monitor is playing. Pass `owner` (callId) to scope it. */
export const isDownlinkMonitorActive = (owner?: string): boolean =>
  monitor !== null && (owner === undefined || monitor.owner === owner);

/**
 * Start listening to the DOWNLINK (what the remote peer hears back from us
 * is *not* monitored — the remote hears *their* own audio; we audit what the
 * customer receives from us, plus PLC/jitter as it leaves this browser).
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
    debugLog("monitor", "warn", "monitor not started: no remoteStream yet", { owner, sinkId: opts.sinkId ?? null });
    return false;
  }
  // Hard anti-loop: never monitor the uplink (own mic).
  if (stream === conn.micStream) {
    console.error("[monitor] refused: remoteStream is the mic stream (uplink) — that would feed back.");
    debugLog("monitor", "error", "monitor refused: remoteStream is the mic stream (uplink)", { owner });
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
    debugLog("monitor", "warn", "monitor unable to play", { owner, sinkId: opts.sinkId ?? null, ...errFields(err) });
    el.srcObject = null;
    return false;
  }

  const primary = opts.primary ?? null;
  const primaryWasMuted = primary?.muted ?? false;
  if (primary && !primaryWasMuted) primary.muted = true; // single renderer rule

  monitor = { owner, el, primary, primaryWasMuted };
  console.log("[monitor] downlink monitor ON (headphones recommended)");
  debugLog("monitor", "info", "downlink monitor ON (headphones recommended)", {
    owner,
    sinkId: opts.sinkId ?? null,
    primaryMuted: primary ? primary.muted : null,
  });
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
  debugLog("monitor", "info", "downlink monitor OFF", { owner: active.owner });
};

/** Re-route the active monitor (e.g. when the user changes the output device). */
export const setDownlinkMonitorSink = async (sinkId: string | null): Promise<void> => {
  if (!monitor) return;
  await applySinkId(monitor.el, sinkId, "monitor");
};
