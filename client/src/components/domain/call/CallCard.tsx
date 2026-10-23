import { useEffect, useRef, useState } from "react";
import { Headphones, Mic, PhoneOff } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  isDownlinkMonitorActive,
  setDownlinkMonitorSink,
  startDownlinkMonitor,
  stopDownlinkMonitor,
} from "wacalls-sdk";
import { debugLog, errFields } from "@/lib/debug-log";
import { sdkClient } from "@/lib/sdk-client";
import { useSdkCallFor } from "@/hooks/useSdkCall";
import { useDevices } from "@/stores/devices";
import { useEndCall } from "@/hooks/useEndCall";
import { formatCallDuration } from "@/utils/format";
import type { CallStatus, CallSummary } from "@/types/call";

const statusVariant: Record<CallStatus, "success" | "secondary" | "muted"> = {
  connected: "success",
  ringing: "secondary",
  starting: "secondary",
  ended: "muted",
};

/* ------------------------------------------------------------------ *
 * Meters — canvas VU + spectrum fed by ONE AnalyserNode per channel,
 * tapped on the REAL audio graph (mic MediaStream / remote MediaStream).
 *
 * Wiring (deliberately boring, see REPORT):
 *   micStream    -> MediaStreamSource -> AnalyserNode   (never to destination)
 *   remoteStream -> MediaStreamSource -> AnalyserNode   (never to destination)
 * Both sources live in a single AudioContext but are only connected to their
 * own analyser: Web Audio routes audio along graph edges, so sharing a
 * context cannot make one channel bleed into the other.
 * ------------------------------------------------------------------ */

const METER_FLOOR_DB = -60; // bottom of the scale
const METER_FFT_SIZE = 1024; // => 512 bins, within the <=1024 budget
const ATTACK_S = 0.01; // fast attack (~10 ms) like a real VU needle
const RELEASE_S = 0.2; // release ~200 ms
const PEAK_HOLD_MS = 700; // peak-hold marker
const PEAK_FALL_S = 0.5; // ...then falls back over ~500 ms
const TEXT_EVERY_MS = 100; // numeric readout throttling (tracks the bar)
const PALETTE_EVERY_MS = 1000; // re-read CSS vars (theme flips)
const METER_DRAW_MS = 33; // ~30fps: skip analyser reads + canvas work above this
const SEG_W = 3; // LED segment width (CSS px)
const SEG_GAP = 2; // gap between segments
const BAR_H = 14; // LED bar height inside the lane
const PEAK_W = 2; // peak marker width
const MAX_DPR = 2; // cap backing-store density
const IDLE_TEXT = "-\u221E dB";

/** SAÍDA accent (cool hue, == bg-sky-600 of the label dot). */
const OUT_ACCENT = "hsl(199 89% 46%)";
/** Warning zone between the base colour and the clip colour. */
const WARN_ZONE = "hsl(43 96% 56%)";

const readCssHsl = (name: string, fallback: string): string => {
  const raw = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return raw ? `hsl(${raw})` : fallback;
};

type Palette = {
  inBase: string;
  outBase: string;
  warn: string;
  clip: string;
  peak: string;
};

const readPalette = (): Palette => ({
  inBase: readCssHsl("--primary", "hsl(152 60% 40%)"),
  outBase: OUT_ACCENT,
  warn: WARN_ZONE,
  clip: readCssHsl("--destructive", "hsl(0 72% 51%)"),
  peak: readCssHsl("--foreground", "hsl(0 0% 100%)"),
});

type Meter = {
  analyser: AnalyserNode;
  source: MediaStreamAudioSourceNode;
  time: Float32Array<ArrayBuffer>;
  freq: Uint8Array<ArrayBuffer>;
  level: number; // smoothed level 0..1
  peak: number; // peak-hold level 0..1
  peakAt: number;
  textAt: number;
  dbText: string;
};

const createMeter = (ctx: AudioContext, stream: MediaStream): Meter => {
  const source = ctx.createMediaStreamSource(stream);
  const analyser = ctx.createAnalyser();
  analyser.fftSize = METER_FFT_SIZE;
  analyser.smoothingTimeConstant = 0.75;
  analyser.minDecibels = METER_FLOOR_DB;
  analyser.maxDecibels = -10;
  source.connect(analyser); // source -> analyser ONLY (tap, no speakers)
  return {
    analyser,
    source,
    time: new Float32Array(analyser.fftSize),
    freq: new Uint8Array(analyser.frequencyBinCount),
    level: 0,
    peak: 0,
    peakAt: 0,
    textAt: 0,
    dbText: IDLE_TEXT,
  };
};

const disposeMeter = (m: Meter | null): void => {
  if (!m) return;
  try {
    m.source.disconnect();
  } catch {}
  try {
    m.analyser.disconnect();
  } catch {}
};

type CanvasSize = { w: number; h: number; dpr: number };

const sizeCanvas = (cv: HTMLCanvasElement): CanvasSize => {
  const rect = cv.getBoundingClientRect();
  const dpr = Math.min(MAX_DPR, window.devicePixelRatio || 1);
  const w = Math.max(1, Math.round(rect.width));
  const h = Math.max(1, Math.round(rect.height));
  const bw = Math.max(1, Math.round(w * dpr));
  const bh = Math.max(1, Math.round(h * dpr));
  if (cv.width !== bw || cv.height !== bh) {
    cv.width = bw;
    cv.height = bh;
  }
  return { w, h, dpr };
};

const drawMeter = (
  g: CanvasRenderingContext2D,
  m: Meter,
  size: CanvasSize,
  base: string,
  pal: Palette,
): void => {
  const { w, h, dpr } = size;
  if (w < 2 || h < 2) return;
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, h);

  // 1) spectrum hill — one FFT per frame, filled so the LED gaps stay clean
  m.analyser.getByteFrequencyData(m.freq);
  const bars = Math.max(8, Math.min(48, Math.round(w / 6)));
  const bw = w / bars;
  const lastBin = m.freq.length - 1;
  g.globalAlpha = 0.16;
  g.fillStyle = base;
  g.beginPath();
  g.moveTo(0, h);
  for (let i = 0; i < bars; i += 1) {
    const lo = Math.floor(((i / bars) ** 2) * lastBin);
    const hi = Math.max(lo + 1, Math.floor((((i + 1) / bars) ** 2) * lastBin));
    let top = 0;
    for (let j = lo; j < hi && j <= lastBin; j += 1) if (m.freq[j] > top) top = m.freq[j];
    g.lineTo((i + 0.5) * bw, h - (top / 255) * h);
  }
  g.lineTo(w, h);
  g.closePath();
  g.fill();
  g.globalAlpha = 1;

  // 2) LED scale: unlit track first, then the lit portion — one fillStyle per
  // color run (base/warn/clip) instead of per segment (≤4 state changes).
  const barY = Math.round((h - BAR_H) / 2);
  const segs = Math.max(1, Math.floor((w + SEG_GAP) / (SEG_W + SEG_GAP)));
  g.fillStyle = base;
  g.globalAlpha = 0.3;
  for (let i = 0; i < segs; i += 1) g.fillRect(i * (SEG_W + SEG_GAP), barY, SEG_W, BAR_H);
  g.globalAlpha = 1;

  const pos = Math.max(0, Math.min(segs, m.level * segs));
  const whole = Math.floor(pos);
  // (i + 1) / segs > threshold  ⇔  i >= floor(threshold * segs)
  const warnFrom = Math.floor(0.7 * segs);
  const clipFrom = Math.floor(0.88 * segs);
  const drawRun = (from: number, to: number, color: string): void => {
    if (to <= from) return;
    g.fillStyle = color;
    for (let i = from; i < to; i += 1) g.fillRect(i * (SEG_W + SEG_GAP), barY, SEG_W, BAR_H);
  };
  drawRun(0, Math.min(whole, warnFrom), base);
  drawRun(warnFrom, Math.min(whole, clipFrom), pal.warn);
  drawRun(Math.min(clipFrom, whole), whole, pal.clip);
  const frac = pos - whole;
  if (frac > 0 && whole < segs) {
    const fp = (whole + 1) / segs;
    g.fillStyle = fp > 0.88 ? pal.clip : fp > 0.7 ? pal.warn : base;
    g.fillRect(whole * (SEG_W + SEG_GAP), barY, Math.max(1, Math.round(SEG_W * frac)), BAR_H);
  }

  // 3) peak-hold marker
  if (m.peak > 0.002) {
    const px = Math.max(0, Math.min(w - PEAK_W, m.peak * w - PEAK_W));
    g.globalAlpha = 0.9;
    g.fillStyle = pal.peak;
    g.fillRect(px, barY - 3, PEAK_W, BAR_H + 6);
    g.globalAlpha = 1;
  }
};

type MeterRowProps = {
  label: string;
  tone: "in" | "out";
  canvasRef: React.RefObject<HTMLCanvasElement | null>;
  textRef: React.RefObject<HTMLSpanElement | null>;
};

/** One channel of the meter panel: label dot + canvas lane + numeric readout. */
const MeterRow = ({ label, tone, canvasRef, textRef }: MeterRowProps) => (
  <div className="grid grid-cols-[3.25rem_1fr_3.5rem] items-center gap-2 sm:gap-3">
    <span className="flex min-w-0 items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
      <span
        aria-hidden="true"
        className={`h-1.5 w-1.5 shrink-0 rounded-full ${
          tone === "in" ? "bg-primary" : "bg-sky-600"
        }`}
      />
      <span className="truncate">{label}</span>
    </span>
    <span className="block h-8 overflow-hidden rounded-md bg-muted">
      <canvas ref={canvasRef} aria-hidden="true" className="block h-full w-full" />
    </span>
    <span ref={textRef} className="text-right text-[11px] tabular-nums text-muted-foreground">
      {IDLE_TEXT}
    </span>
  </div>
);

export const CallCard = ({ call }: { call: CallSummary }) => {
  // SDK owns the RTCPeerConnection + playback; this hook exposes its handle.
  const conn = useSdkCallFor(call.callId);
  const outDeviceId = useDevices((s) => s.outId);
  const endCall = useEndCall();
  const [, force] = useState(0);
  const [muted, setMuted] = useState(false);
  const [monitorWanted, setMonitorWanted] = useState(false);
  const aliveRef = useRef(true);
  const micCanvasRef = useRef<HTMLCanvasElement>(null);
  const peerCanvasRef = useRef<HTMLCanvasElement>(null);
  const micTextRef = useRef<HTMLSpanElement>(null);
  const peerTextRef = useRef<HTMLSpanElement>(null);

  // Live view: another call card (or a failed start) may have stopped it.
  const monitorOn = monitorWanted && isDownlinkMonitorActive(call.callId);

  // Duration badge ticker (not the meters — they run on rAF below).
  useEffect(() => {
    const t = setInterval(() => force((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, []);

  // Mute: local state is only an optimistic render cache — the SDK's desired
  // mute state (sdkClient.isMuted) is the source of truth. It survives call
  // changes and is applied to freshly opened mics, so we reconcile from the
  // getter on call change and after every toggle (the toggle can no-op while
  // the media is still opening; the getter never lies).
  useEffect(() => {
    setMuted(sdkClient.isMuted);
  }, [call.callId]);

  // Never leak a running monitor past this card (unmount / call ended).
  useEffect(() => {
    aliveRef.current = true;
    const id = call.callId;
    return () => {
      aliveRef.current = false;
      stopDownlinkMonitor(id);
    };
  }, [call.callId]);

  // Field instrumentation: mic track + playback state every ~2 s while the
  // call is up (settings can change mid-call; the one-shot gum log at open
  // can't see it). Cleans up on unmount / call end (conn goes away). The
  // primary downlink <audio> is owned by the SDK (activeAudio).
  useEffect(() => {
    if (!conn) return;
    const id = call.callId;
    const tick = setInterval(() => {
      const track = conn.micStream?.getAudioTracks()[0] ?? null;
      const s = track?.getSettings();
      const el = sdkClient.activeAudio as (HTMLAudioElement & { sinkId?: string }) | null;
      debugLog("client:track", "debug", "track state", {
        callId: id,
        track: track
          ? { enabled: track.enabled, muted: track.muted, readyState: track.readyState }
          : null,
        echoCancellation: s?.echoCancellation ?? null,
        noiseSuppression: s?.noiseSuppression ?? null,
        autoGainControl: s?.autoGainControl ?? null,
        deviceId: s?.deviceId ?? null,
        sampleRate: s?.sampleRate ?? null,
        channelCount: s?.channelCount ?? null,
        audioCtxState: conn.ctx.state,
        primaryMuted: el?.muted ?? null,
        monitorActive: isDownlinkMonitorActive(id),
        sinkId: el?.sinkId ?? null,
      });
    }, 2000);
    return () => clearInterval(tick);
  }, [conn, call.callId]);

  // Meters: one AudioContext, one AnalyserNode per channel, one rAF loop.
  useEffect(() => {
    if (!conn) return;
    const micCanvas = micCanvasRef.current;
    const peerCanvas = peerCanvasRef.current;
    const micText = micTextRef.current;
    const peerText = peerTextRef.current;
    if (!micCanvas || !peerCanvas || !micText || !peerText) return;

    micText.textContent = IDLE_TEXT;
    peerText.textContent = IDLE_TEXT;

    let meterCtx: AudioContext;
    try {
      meterCtx = new AudioContext();
    } catch (err) {
      debugLog("audio", "error", "meter AudioContext failed", { callId: call.callId, ...errFields(err) });
      return;
    }
    void meterCtx.resume().catch(() => {});

    type Channel = {
      tone: "in" | "out";
      canvas: HTMLCanvasElement;
      text: HTMLElement;
      getStream: () => MediaStream | null;
      meter: Meter | null;
    };
    const channels: Channel[] = [
      { tone: "in", canvas: micCanvas, text: micText, getStream: () => conn.micStream, meter: null },
      {
        tone: "out",
        canvas: peerCanvas,
        text: peerText,
        // remoteStream is typed nullable: the peer analyser is created lazily
        // by the loop below as soon as it shows up.
        getStream: () => conn.remoteStream,
        meter: null,
      },
    ];

    const sizes = new Map<HTMLCanvasElement, CanvasSize>();
    const ro = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const cv = entry.target as HTMLCanvasElement;
        sizes.set(cv, sizeCanvas(cv));
      }
    });
    ro.observe(micCanvas);
    ro.observe(peerCanvas);

    let pal = readPalette();
    let palAt = 0;
    let last = -1;
    let raf = 0;

    const frame = (t: number) => {
      raf = requestAnimationFrame(frame);
      if (document.hidden) return; // idle while the tab is hidden
      // ~30fps cap: skip the whole channel loop (analyser reads, RMS, canvas)
      // between draws. `last` only advances on drawn frames, so `dt` below
      // spans the skipped frames and the ballistics stay time-based (clamped).
      if (last >= 0 && t - last < METER_DRAW_MS) return;
      const dt = last < 0 ? 0 : Math.min(0.1, (t - last) / 1000);
      last = t;
      if (t - palAt >= PALETTE_EVERY_MS) {
        pal = readPalette();
        palAt = t;
      }

      for (const ch of channels) {
        if (!ch.meter) {
          const stream = ch.getStream();
          if (!stream) continue;
          ch.meter = createMeter(meterCtx, stream);
        }
        const m = ch.meter;

        // Level from the time-domain signal (RMS -> dBFS -> 0..1)
        m.analyser.getFloatTimeDomainData(m.time);
        let sum = 0;
        for (let i = 0; i < m.time.length; i += 1) sum += m.time[i] * m.time[i];
        const rms = Math.sqrt(sum / m.time.length);
        const db =
          rms > 0 ? Math.max(METER_FLOOR_DB, Math.min(0, 20 * Math.log10(rms))) : METER_FLOOR_DB;
        const target = (db - METER_FLOOR_DB) / -METER_FLOOR_DB;

        // VU ballistics: fast attack, ~200 ms release
        const tau = target > m.level ? ATTACK_S : RELEASE_S;
        m.level += (target - m.level) * (1 - Math.exp(-dt / tau));

        // Peak hold + fall
        if (m.level >= m.peak) {
          m.peak = m.level;
          m.peakAt = t;
        } else if (t - m.peakAt > PEAK_HOLD_MS) {
          m.peak = Math.max(m.level, m.peak - dt / PEAK_FALL_S);
        }

        // Numeric readout (textual alternative), throttled.
        // Reports the SAME smoothed level the bar draws, so they never disagree.
        if (t - m.textAt >= TEXT_EVERY_MS) {
          m.textAt = t;
          const shownDb = METER_FLOOR_DB + m.level * -METER_FLOOR_DB;
          const next = `${shownDb <= METER_FLOOR_DB + 0.5 ? "-\u221E" : Math.round(shownDb)} dB`;
          if (next !== m.dbText) {
            m.dbText = next;
            ch.text.textContent = next;
          }
        }

        let size = sizes.get(ch.canvas);
        const dpr = Math.min(MAX_DPR, window.devicePixelRatio || 1);
        if (!size || size.dpr !== dpr) {
          size = sizeCanvas(ch.canvas);
          sizes.set(ch.canvas, size);
        }
        const ctx2d = ch.canvas.getContext("2d");
        if (ctx2d) drawMeter(ctx2d, m, size, ch.tone === "in" ? pal.inBase : pal.outBase, pal);
      }
    };
    raf = requestAnimationFrame(frame);

    return () => {
      cancelAnimationFrame(raf);
      ro.disconnect();
      for (const ch of channels) disposeMeter(ch.meter);
      try {
        meterCtx.close();
      } catch {}
    };
  }, [conn]);

  useEffect(() => {
    // The SDK owns the primary downlink <audio>: it applies outId on open and
    // re-routes live via sdkClient.setOutId (devices store subscription).
    // Keep the monitor on the same output device as the primary audio.
    if (isDownlinkMonitorActive(call.callId)) void setDownlinkMonitorSink(outDeviceId);
  }, [outDeviceId, call.callId]);

  const toggleMonitor = () => {
    if (!conn?.remoteStream) return;
    if (monitorOn) {
      stopDownlinkMonitor(call.callId);
      setMonitorWanted(false);
      return;
    }
    setMonitorWanted(true);
    // primary: the SDK's internal playback element — the monitor mutes it
    // while active (single-renderer rule, no comb-filter double playback).
    void startDownlinkMonitor(call.callId, conn, {
      sinkId: outDeviceId,
      primary: sdkClient.activeAudio,
    }).then((ok) => {
      if (ok && aliveRef.current) return;
      stopDownlinkMonitor(call.callId);
      setMonitorWanted(false);
    });
  };

  const toggleMute = () => {
    const next = !muted;
    setMuted(next); // optimistic — reconciled immediately below
    sdkClient.toggleMute(next);
    setMuted(sdkClient.isMuted); // SDK is the source of truth
  };

  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="truncate font-medium">{call.peer}</p>
            <Badge variant={statusVariant[call.status]} className="mt-1">
              {formatCallDuration(call.startedAt, call.status)}
            </Badge>
          </div>
          <div className="flex items-center gap-2">
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant={monitorOn ? "secondary" : "outline"}
                  size="icon"
                  onClick={toggleMonitor}
                  disabled={!conn?.remoteStream}
                  aria-label="Monitor downlink"
                  aria-pressed={monitorOn}
                >
                  <Headphones className="h-4 w-4" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>
                Monitor: escute o que o cliente recebe (downlink) — use fone ao monitorar
              </TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant={muted ? "secondary" : "outline"}
                  size="icon"
                  onClick={toggleMute}
                  disabled={!conn}
                  aria-label={muted ? "Unmute microphone" : "Mute microphone"}
                  aria-pressed={muted}
                >
                  <Mic className="h-4 w-4" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{muted ? "Unmute microphone" : "Mute microphone"}</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="destructive"
                  size="icon"
                  onClick={() => endCall.mutate({ sid: call.sessionId, callId: call.callId })}
                  aria-label="End call"
                >
                  <PhoneOff className="h-4 w-4" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>End call</TooltipContent>
            </Tooltip>
          </div>
        </div>
        {monitorOn && (
          <p className="text-xs text-muted-foreground">
            Monitor ativo — use fone: no mesmo speaker o downlink volta pelo mic e gera eco.
          </p>
        )}
        <div className="space-y-2" role="group" aria-label="Audio levels">
          <MeterRow label="Mic" tone="in" canvasRef={micCanvasRef} textRef={micTextRef} />
          <MeterRow label="Peer" tone="out" canvasRef={peerCanvasRef} textRef={peerTextRef} />
        </div>
      </CardContent>
    </Card>
  );
};
