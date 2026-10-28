import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { Copy, Terminal, TriangleAlert, X } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ApiError, apiFetch } from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  ALERT_SOURCES,
  LEVEL_RANK,
  SERVER_SOURCES,
  getDebugLogSnapshot,
  isDebugLevel,
  subscribeDebugLog,
  type DebugEvent,
} from "@/lib/debug-log";
import { useUi } from "@/stores/ui";

/* -------------------------------------------------------------------------- *
 * Tuning
 * -------------------------------------------------------------------------- */

const POLL_MS = 1500; // 1–2 s auto-poll while the panel is open
const PAGE_LIMIT = 100; // ?limit= per request
const MAX_BURST = 20; // requests per tick while catching up with the ring
const FETCH_TIMEOUT_MS = 5000;
const MAX_ROWS = 500; // render budget (newest N)
const MAX_SERVER_EVENTS = 1500; // bounded client-side copy of the ~2000 ring

type SourceFilter = "all" | "client" | (typeof SERVER_SOURCES)[number];
type LevelFilter = "all" | "info" | "warn" | "error";

const LEVEL_CHIPS: { key: LevelFilter; label: string; min: number }[] = [
  { key: "all", label: "todos", min: 0 },
  { key: "info", label: "info+", min: 1 },
  { key: "warn", label: "warn+", min: 2 },
  { key: "error", label: "error", min: 3 },
];

/* -------------------------------------------------------------------------- *
 * Helpers
 * -------------------------------------------------------------------------- */

const pad = (n: number, w = 2): string => String(n).padStart(w, "0");

const formatAbsolute = (ts: number): string => {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "—";
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
    `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`
  );
};

/** Compact relative age; `title`/sr-only carries the absolute timestamp. */
const formatRelative = (ts: number, now: number): string => {
  const delta = now - ts;
  const abs = Math.abs(delta) / 1000;
  const sign = delta < 0 ? "+" : "";
  if (abs < 1) return "now";
  if (abs < 60) return `${sign}${Math.floor(abs)}s`;
  if (abs < 3600) return `${sign}${Math.floor(abs / 60)}m`;
  if (abs < 86400) return `${sign}${Math.floor(abs / 3600)}h`;
  return `${sign}${Math.floor(abs / 86400)}d`;
};

const parseTimestamp = (ts: unknown): number => {
  if (typeof ts === "number" && Number.isFinite(ts)) return ts < 1e12 ? Math.round(ts * 1000) : ts;
  if (typeof ts === "string") {
    const parsed = Date.parse(ts);
    if (!Number.isNaN(parsed)) return parsed;
  }
  return Date.now();
};

/** Map one backend ring event (`internal/voip/logbuf.Event`) to a DebugEvent. */
const toServerEvent = (raw: unknown): DebugEvent | null => {
  if (typeof raw !== "object" || raw === null) return null;
  const e = raw as Record<string, unknown>;
  if (typeof e.seq !== "number" || !Number.isFinite(e.seq)) return null;
  if (typeof e.msg !== "string") return null;
  const fields =
    typeof e.fields === "object" && e.fields !== null && !Array.isArray(e.fields)
      ? (e.fields as Record<string, unknown>)
      : undefined;
  return {
    id: `s${e.seq}`,
    origin: "server",
    seq: e.seq,
    ts: parseTimestamp(e.ts),
    source: typeof e.source === "string" && e.source.length > 0 ? e.source : "session",
    level: isDebugLevel(e.level) ? e.level : "info",
    msg: e.msg,
    ...(fields ? { fields } : {}),
  };
};

const parsePayload = (
  data: unknown,
): { events: DebugEvent[]; nextSeq: number | null } => {
  if (typeof data !== "object" || data === null) throw new Error("unexpected payload");
  const d = data as { events?: unknown; nextSeq?: unknown };
  if (!Array.isArray(d.events)) throw new Error("unexpected payload");
  const events = d.events
    .map(toServerEvent)
    .filter((e): e is DebugEvent => e !== null);
  const nextSeq = typeof d.nextSeq === "number" && Number.isFinite(d.nextSeq) ? d.nextSeq : null;
  return { events, nextSeq };
};

const matchesSource = (filter: SourceFilter, source: string): boolean => {
  if (filter === "all") return true;
  if (filter === "client") return source.startsWith("client:");
  // `uplink` also matches the local `client:uplink` counterpart.
  return source === filter || source === `client:${filter}`;
};

const stringifyFields = (fields: Record<string, unknown>): string => {
  try {
    return JSON.stringify(fields);
  } catch {
    return "[unserializable]";
  }
};

/* -------------------------------------------------------------------------- *
 * Presentational bits
 * -------------------------------------------------------------------------- */

const Chip = ({
  active,
  alert,
  onClick,
  children,
}: {
  active: boolean;
  alert?: boolean;
  onClick: () => void;
  children: ReactNode;
}) => (
  <button
    type="button"
    aria-pressed={active}
    onClick={onClick}
    className={cn(
      "rounded-full border px-2.5 py-1 text-[11px] font-medium transition-colors",
      "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
      active
        ? "border-primary bg-primary text-primary-foreground"
        : alert
          ? "border-orange-500/50 bg-background text-orange-700 dark:text-orange-400"
          : "border-border bg-background text-muted-foreground hover:bg-accent hover:text-accent-foreground",
    )}
  >
    {children}
  </button>
);

const levelBadge: Record<DebugEvent["level"], string> = {
  debug: "border-border bg-muted text-muted-foreground",
  info: "border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-400",
  warn: "border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  error: "border-red-500/50 bg-red-500/10 text-red-700 dark:text-red-400",
};

/** warn/error rows are highlighted by a border + tint AND their text badge. */
const rowTone: Record<DebugEvent["level"], string> = {
  debug: "border-l-border",
  info: "border-l-border",
  warn: "border-l-amber-500 bg-amber-500/5",
  error: "border-l-red-500 bg-red-500/5",
};

const LogRow = ({ ev, now }: { ev: DebugEvent; now: number }) => {
  const absolute = formatAbsolute(ev.ts);
  const fieldsJson = ev.fields ? stringifyFields(ev.fields) : "";
  const alert = ALERT_SOURCES.has(ev.source);

  return (
    <li className={cn("flex flex-wrap items-start gap-x-3 gap-y-1 border-l-2 px-2 py-1.5", rowTone[ev.level])}>
      <time
        dateTime={new Date(ev.ts).toISOString()}
        title={absolute}
        className="w-[3.75rem] shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground"
      >
        {formatRelative(ev.ts, now)}
        <span className="sr-only"> ({absolute})</span>
      </time>
      <span
        className={cn(
          "w-[3.25rem] shrink-0 rounded border px-1 text-center text-[10px] font-semibold uppercase tracking-wide",
          levelBadge[ev.level],
        )}
      >
        {ev.level}
      </span>
      <span
        title={alert ? "Signal for duplicated audio / echo / loop" : ev.source}
        className={cn(
          "inline-flex shrink-0 items-center gap-1 font-mono text-[11px]",
          ev.origin === "local" && "text-foreground/80",
          alert && "text-orange-700 dark:text-orange-400",
        )}
      >
        {alert && <TriangleAlert className="h-3 w-3" aria-hidden="true" />}
        {alert && <span className="sr-only">possible duplicate or echo signal: </span>}
        {ev.source}
      </span>
      <span className="min-w-0 flex-1 break-words text-xs leading-5">
        {ev.msg}
        {fieldsJson && (
          <span title={fieldsJson} className="mt-0.5 block truncate font-mono text-[11px] text-muted-foreground">
            {fieldsJson}
          </span>
        )}
      </span>
    </li>
  );
};

/* -------------------------------------------------------------------------- *
 * Page
 * -------------------------------------------------------------------------- */

export const DebugPage = () => {
  const setView = useUi((s) => s.setView);

  const [serverEvents, setServerEvents] = useState<DebugEvent[]>([]);
  const [backend, setBackend] = useState<"unknown" | "ok" | "waiting" | "expired">("unknown");
  const [paused, setPaused] = useState(() => typeof document !== "undefined" && document.hidden);
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>("all");
  const [levelFilter, setLevelFilter] = useState<LevelFilter>("all");
  const [query, setQuery] = useState("");
  const [now, setNow] = useState(() => Date.now());

  const localEvents = useSyncExternalStore(subscribeDebugLog, getDebugLogSnapshot, getDebugLogSnapshot);

  const aliveRef = useRef(true);
  const busyRef = useRef(false);
  const seqRef = useRef(0);
  const abortRef = useRef<AbortController | null>(null);

  /* --- polling ----------------------------------------------------------- */

  const pollOnce = useCallback(async (): Promise<number> => {
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    const timer = window.setTimeout(() => ctrl.abort(), FETCH_TIMEOUT_MS);
    try {
      const res = await apiFetch(`/api/debug/logs?since=${seqRef.current}&limit=${PAGE_LIMIT}`, {
        signal: ctrl.signal,
        headers: { Accept: "application/json" },
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const { events, nextSeq } = parsePayload(await res.json());
      if (!aliveRef.current) return events.length;

      // Cursor: trust nextSeq; fall back to the last returned seq (and to the
      // current cursor when nothing came back, so a payload without nextSeq
      // can never reset an already-populated list).
      const returned = events.length > 0 ? (events[events.length - 1].seq ?? 0) : seqRef.current;
      const next = nextSeq ?? returned;
      if (next < seqRef.current) {
        // Backend ring restarted (seq reset): drop stale rows, restart cursor.
        setServerEvents([]);
      }
      seqRef.current = next;

      if (events.length > 0) {
        setServerEvents((prev) => {
          const seen = new Set<number>();
          for (const e of prev) if (e.seq !== undefined) seen.add(e.seq);
          const fresh = events.filter((e) => e.seq !== undefined && !seen.has(e.seq));
          if (fresh.length === 0) return prev;
          const mergedEvents = prev.concat(fresh);
          mergedEvents.sort((a, b) => a.ts - b.ts || (a.seq ?? 0) - (b.seq ?? 0));
          return mergedEvents.length > MAX_SERVER_EVENTS
            ? mergedEvents.slice(-MAX_SERVER_EVENTS)
            : mergedEvents;
        });
      }
      setBackend("ok");
      return events.length;
    } catch (err) {
      // A 401 is the session expiring, not a backend failure: apiFetch has
      // already run hangup/logout and will redirect to the login gate.
      if (err instanceof ApiError && err.status === 401) {
        if (aliveRef.current) setBackend("expired");
        return -1;
      }
      // Endpoint missing, backend down or timeout → degrade gracefully.
      if (aliveRef.current) setBackend("waiting");
      return -1;
    } finally {
      window.clearTimeout(timer);
      if (abortRef.current === ctrl) abortRef.current = null;
    }
  }, []);

  useEffect(() => {
    aliveRef.current = true;
    let stopped = false;
    let timer: number | null = null;

    const tick = async () => {
      if (stopped || busyRef.current || document.hidden) return;
      busyRef.current = true;
      try {
        // Catch up with the ring in one tick (limit=100 per request).
        for (let i = 0; i < MAX_BURST; i += 1) {
          const received = await pollOnce();
          if (stopped || received < 0) break;
          if (received < PAGE_LIMIT) break;
        }
      } finally {
        busyRef.current = false;
      }
    };

    const start = () => {
      if (timer !== null || stopped) return;
      timer = window.setInterval(() => void tick(), POLL_MS);
    };
    const stop = () => {
      if (timer === null) return;
      window.clearInterval(timer);
      timer = null;
    };
    const onVisibility = () => {
      const hidden = document.hidden;
      setPaused(hidden);
      if (hidden) stop();
      else {
        start();
        void tick();
      }
    };

    document.addEventListener("visibilitychange", onVisibility);
    setPaused(document.hidden);
    start();
    void tick();

    return () => {
      stopped = true;
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
      abortRef.current?.abort();
      abortRef.current = null;
      aliveRef.current = false;
    };
  }, [pollOnce]);

  /* --- relative clock (1 s) ---------------------------------------------- */

  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(t);
  }, []);

  /* --- merge + filter ----------------------------------------------------- */

  const activeLevel = LEVEL_CHIPS.find((c) => c.key === levelFilter) ?? LEVEL_CHIPS[0];

  const merged = useMemo(() => {
    const all = localEvents.concat(serverEvents);
    all.sort((a, b) => a.ts - b.ts || (a.seq ?? 0) - (b.seq ?? 0));
    return all;
  }, [localEvents, serverEvents]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return merged.filter((ev) => {
      if (!matchesSource(sourceFilter, ev.source)) return false;
      if (LEVEL_RANK[ev.level] < activeLevel.min) return false;
      if (!q) return true;
      const haystack = `${ev.source} ${ev.msg} ${ev.fields ? stringifyFields(ev.fields) : ""}`.toLowerCase();
      return haystack.includes(q);
    });
  }, [merged, sourceFilter, activeLevel, query]);

  const rows = useMemo(() => {
    const visible =
      filtered.length > MAX_ROWS ? filtered.slice(filtered.length - MAX_ROWS) : filtered;
    return visible.slice().reverse(); // newest first ⇒ no scroll chasing
  }, [filtered]);

  /* --- export ------------------------------------------------------------- */

  const copyJson = async () => {
    if (filtered.length === 0) return;
    const payload = filtered.map((ev) => ({ ...ev, iso: new Date(ev.ts).toISOString() }));
    try {
      await navigator.clipboard.writeText(JSON.stringify(payload, null, 2));
      toast.success(`Copied ${payload.length} event${payload.length === 1 ? "" : "s"} as JSON`);
    } catch {
      toast.error("Clipboard unavailable — copy blocked by the browser");
    }
  };

  /* --- render ------------------------------------------------------------- */

  return (
    <section aria-label="Debug logs" className="mx-auto flex max-w-5xl flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="flex items-center gap-2 text-lg font-semibold tracking-tight">
            <Terminal className="h-4 w-4" aria-hidden="true" />
            Debug
          </h2>
          <p className="text-xs text-muted-foreground">
            Audio trail: seasonal echo (“client hears itself”), duplicated audio and what the mic sends.
            Server ring + local <code className="font-mono">client:*</code> logs, merged by timestamp.
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => void copyJson()} disabled={filtered.length === 0}>
            <Copy className="h-4 w-4" aria-hidden="true" />
            Copy
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setView("app")} aria-label="Close debug panel">
            <X className="h-4 w-4" aria-hidden="true" />
            Close
          </Button>
        </div>
      </header>

      <div className="flex flex-wrap items-center gap-2 text-[11px]">
        <span
          role="status"
          className={cn(
            "rounded-full border px-2 py-0.5 font-medium",
            backend === "ok"
              ? "border-primary/40 bg-primary/10 text-primary"
              : backend === "expired"
                ? "border-destructive/50 bg-destructive/10 text-destructive"
                : "border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-400",
          )}
        >
          {backend === "ok"
            ? `backend ok · ${serverEvents.length} events`
            : backend === "expired"
              ? "session expired — redirecting to sign in"
              : "aguardando backend"}
        </span>
        <span className="rounded-full border border-border px-2 py-0.5 font-medium text-muted-foreground">
          local {localEvents.length}
        </span>
        {paused && (
          <span className="rounded-full border border-border px-2 py-0.5 font-medium text-muted-foreground">
            polling paused (tab hidden)
          </span>
        )}
        <span className="text-muted-foreground">
          showing {rows.length} of {filtered.length} matching · {merged.length} buffered
        </span>
      </div>

      <div className="space-y-2" role="group" aria-label="Log filters">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="mr-1 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Source
          </span>
          <Chip active={sourceFilter === "all"} onClick={() => setSourceFilter("all")}>
            TODOS
          </Chip>
          {SERVER_SOURCES.map((s) => (
            <Chip key={s} active={sourceFilter === s} alert={ALERT_SOURCES.has(s)} onClick={() => setSourceFilter(s)}>
              {s}
            </Chip>
          ))}
          <Chip active={sourceFilter === "client"} onClick={() => setSourceFilter("client")}>
            client:*
          </Chip>
        </div>

        <div className="flex flex-wrap items-center gap-1.5">
          <span className="mr-1 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Level
          </span>
          {LEVEL_CHIPS.map((c) => (
            <Chip key={c.key} active={levelFilter === c.key} onClick={() => setLevelFilter(c.key)}>
              {c.label}
            </Chip>
          ))}
          <div className="ml-auto flex min-w-0 items-center gap-2">
            <label htmlFor="debug-log-search" className="sr-only">
              Search message and fields
            </label>
            <Input
              id="debug-log-search"
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search msg / fields…"
              className="h-8 w-full max-w-56 text-xs sm:w-56"
            />
          </div>
        </div>
        <p className="text-[11px] text-muted-foreground">
          Level filter: {activeLevel.min === 0 ? "all levels" : `${activeLevel.label} and above`}
        </p>
      </div>

      {rows.length > 0 && (
        <ol
          role="log"
          aria-live="off"
          aria-label="Debug events, newest first"
          className="max-h-[65vh] divide-y overflow-y-auto rounded-md border bg-card"
        >
          {rows.map((ev) => (
            <LogRow key={ev.id} ev={ev} now={now} />
          ))}
        </ol>
      )}

      {rows.length === 0 && (
        <p className="rounded-md border border-dashed px-3 py-6 text-center text-sm text-muted-foreground">
          {merged.length === 0
            ? backend === "waiting"
              ? "Aguardando backend — no server events yet; local client:* events appear here as soon as they happen."
              : "No events yet — open a call to see what the mic sends."
            : "No events match the current filters."}
        </p>
      )}

      {filtered.length > MAX_ROWS && (
        <p role="status" className="text-xs text-amber-700 dark:text-amber-400">
          Rendering the {MAX_ROWS} most recent lines of {filtered.length} — narrow the filters to reach older
          entries (full list is still exported by Copy).
        </p>
      )}
    </section>
  );
};
