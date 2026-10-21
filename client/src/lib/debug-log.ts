/**
 * ---------------------------------------------------------------------------
 * Local (browser-side) debug log ring — client counterpart of the backend
 * `GET /api/debug/logs` ring buffer (`internal/voip/logbuf`).
 * ---------------------------------------------------------------------------
 *
 * The DEBUG page merges two sources of truth in one timeline:
 *
 *   origin "server"  — polled from the backend (sources: uplink, downlink,
 *                      relay-filter, srtp, bridge, dup-detector, session)
 *   origin "local"   — pushed from browser code through `debugLog()`, every
 *                      entry gets the `client:` prefix (client:gum, client:dc,
 *                      client:uplink, client:downlink, client:plc,
 *                      client:monitor, client:sink, client:audio) so it can
 *                      never be confused with a server source of the same
 *                      name (e.g. local `uplink` vs server `uplink`).
 *
 * `debugLog` only writes to the ring — existing `console.*` calls elsewhere in
 * the app are kept as-is (no double console output, no removed logs).
 */

export type DebugLevel = "debug" | "info" | "warn" | "error";
export type DebugOrigin = "local" | "server";

/** One log entry, shared by the local ring and the polled server ring. */
export type DebugEvent = {
  /** Stable React key: `l<n>` local, `s<seq>` server. */
  id: string;
  origin: DebugOrigin;
  /** Backend ring cursor (server events only). */
  seq?: number;
  /** Epoch milliseconds. */
  ts: number;
  source: string;
  level: DebugLevel;
  msg: string;
  fields?: Record<string, unknown>;
};

export const CLIENT_SOURCE_PREFIX = "client:";
export const LOCAL_RING_SIZE = 500;

/** Sources emitted by the backend (chips on the DEBUG page). */
export const SERVER_SOURCES = [
  "uplink",
  "downlink",
  "relay-filter",
  "srtp",
  "bridge",
  "dup-detector",
  "session",
] as const;

/** Sources that signal duplicated audio / echo / loop problems. */
export const ALERT_SOURCES = new Set(["dup-detector", "srtp", "relay-filter"]);

export const LEVEL_RANK: Record<DebugLevel, number> = { debug: 0, info: 1, warn: 2, error: 3 };

export const isDebugLevel = (v: unknown): v is DebugLevel =>
  v === "debug" || v === "info" || v === "warn" || v === "error";

/* -------------------------------------------------------------------------- */
/* Ring                                                                        */
/* -------------------------------------------------------------------------- */

let localCounter = 0;
let ring: DebugEvent[] = [];
let version = 0;
let cached: DebugEvent[] = [];
let cachedVersion = -1;
const listeners = new Set<() => void>();
let notifyTimer: ReturnType<typeof setTimeout> | null = null;

/** Coalesce change notifications so a chatty ring cannot thrash React. */
const scheduleNotify = (): void => {
  if (notifyTimer !== null) return;
  const schedule = typeof window !== "undefined" ? window.setTimeout.bind(window) : setTimeout;
  notifyTimer = schedule(() => {
    notifyTimer = null;
    for (const l of listeners) l();
  }, 200);
};

/**
 * Make `fields` JSON-safe and bounded: drops cycles, binary blobs, class
 * instances and deep nesting instead of throwing while serialising.
 */
const sanitize = (value: unknown, depth = 0): unknown => {
  if (value === null || value === undefined) return value ?? null;
  const t = typeof value;
  if (t === "string" || t === "number" || t === "boolean") return value;
  if (t === "bigint") return `${value as bigint}n`;
  if (t === "function" || t === "symbol") return String(value);
  if (value instanceof Error) return { name: value.name, message: value.message };
  if (value instanceof Date) return value.toISOString();
  if (ArrayBuffer.isView(value)) return `[${(value as ArrayBufferView).byteLength} bytes]`;
  if (value instanceof ArrayBuffer) return `[${value.byteLength} bytes]`;
  if (depth >= 5) return "[depth-limit]";
  if (Array.isArray(value)) return value.slice(0, 50).map((v) => sanitize(v, depth + 1));
  if (t === "object") {
    const out: Record<string, unknown> = {};
    let n = 0;
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      if (n >= 50) {
        out["…"] = "truncated";
        break;
      }
      out[k] = sanitize(v, depth + 1);
      n += 1;
    }
    return out;
  }
  return String(value);
};

const sanitizeFields = (fields: Record<string, unknown>): Record<string, unknown> =>
  sanitize(fields) as Record<string, unknown>;

/** Normalised `{name, message}` view of anything throwable. */
export const errFields = (err: unknown): Record<string, unknown> => {
  if (err instanceof Error) return { name: err.name, message: err.message };
  if (typeof err === "object" && err !== null && !Array.isArray(err)) {
    return sanitizeFields(err as Record<string, unknown>);
  }
  return { error: String(err) };
};

const push = (source: string, level: DebugLevel, msg: string, fields?: Record<string, unknown>): void => {
  const ev: DebugEvent = {
    id: `l${(localCounter += 1)}`,
    origin: "local",
    ts: Date.now(),
    source: source.startsWith(CLIENT_SOURCE_PREFIX) ? source : `${CLIENT_SOURCE_PREFIX}${source}`,
    level,
    msg,
    ...(fields ? { fields: sanitizeFields(fields) } : {}),
  };
  ring.push(ev);
  if (ring.length > LOCAL_RING_SIZE) ring.splice(0, ring.length - LOCAL_RING_SIZE);
  version += 1;
  scheduleNotify();
};

/**
 * Append one local event to the ring.
 *
 * @param source bare subsystem name (`gum`, `dc`, `uplink`, `monitor`, …);
 *               the `client:` prefix is applied automatically.
 */
export const debugLog = (
  source: string,
  level: DebugLevel,
  msg: string,
  fields?: Record<string, unknown>,
): void => {
  push(source, level, msg, fields);
};

/* -------------------------------------------------------------------------- */
/* Rate-limited logging (2/s per key)                                          */
/* -------------------------------------------------------------------------- */

type RateBucket = { at: number; count: number; bytes: number };
const rateBuckets = new Map<string, RateBucket>();

/**
 * Emit at most one entry per `intervalMs` (default 500 ms ⇒ 2/s) for `key`;
 * the activity in between is folded into the emitted entry as
 * `{bytes, msgs, windowMs}` plus any `extra` fields from the caller (values
 * from the frame that triggered the emission). Used by the dc.send /
 * dc.onmessage hot paths so they cannot flood the ring.
 */
export const debugLogRate = (opts: {
  key: string;
  source: string;
  level: DebugLevel;
  msg: string;
  bytes?: number;
  intervalMs?: number;
  extra?: Record<string, unknown>;
}): void => {
  const intervalMs = opts.intervalMs ?? 500;
  const bytes = opts.bytes ?? 0;
  const now = Date.now();
  const bucket = rateBuckets.get(opts.key);
  if (!bucket) {
    rateBuckets.set(opts.key, { at: now, count: 0, bytes: 0 });
    push(opts.source, opts.level, opts.msg, { ...opts.extra, bytes, msgs: 1, windowMs: 0 });
    return;
  }
  bucket.count += 1;
  bucket.bytes += bytes;
  if (now - bucket.at < intervalMs) return;
  const fields = { ...opts.extra, bytes: bucket.bytes, msgs: bucket.count, windowMs: now - bucket.at };
  bucket.at = now;
  bucket.count = 0;
  bucket.bytes = 0;
  push(opts.source, opts.level, opts.msg, fields);
};

/* -------------------------------------------------------------------------- */
/* Store (useSyncExternalStore)                                                */
/* -------------------------------------------------------------------------- */

/** Subscribe to local ring changes (notifications are coalesced ~200 ms). */
export const subscribeDebugLog = (listener: () => void): (() => void) => {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
};

/** Immutable snapshot — reference only changes when the ring changes. */
export const getDebugLogSnapshot = (): DebugEvent[] => {
  if (cachedVersion !== version) {
    cached = ring.slice();
    cachedVersion = version;
  }
  return cached;
};
