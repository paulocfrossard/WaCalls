import { useState, type FormEvent } from "react";
import { Copy, Settings } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useUi } from "@/stores/ui";
import { reconfigureSdk } from "@/lib/sdk-client";
import {
  clearBaseUrl,
  isSessionOnlyBase,
  normalizeBaseUrl,
  persistBaseUrl,
  resolveBaseUrl,
  resolveBaseUrlDetailed,
  withBase,
} from "@/lib/base";

const resolveSource = (): "panel" | "build" | "fallback" => resolveBaseUrlDetailed().source;

export const SettingsPage = () => {
  const setView = useUi((s) => s.setView);
  const [draft, setDraft] = useState(() => resolveBaseUrl());
  const [error, setError] = useState<string | null>(null);
  const [storageWarning, setStorageWarning] = useState(() => isSessionOnlyBase());
  const [effective, setEffective] = useState(() => resolveBaseUrl());
  const [source, setSource] = useState<"panel" | "build" | "fallback">(() => resolveSource());

  const handleSave = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const result = normalizeBaseUrl(draft);
    if (!result.ok) {
      setError(result.error);
      return;
    }
    setError(null);
    const persisted = persistBaseUrl(result.value);
    const sessionOnly = persisted.ok && "sessionOnly" in persisted && persisted.sessionOnly === true;
    if (sessionOnly) {
      setStorageWarning(true);
    } else {
      setStorageWarning(false);
    }
    reconfigureSdk(result.value);
    const next = resolveBaseUrl();
    setDraft(next);
    setEffective(next);
    setSource(resolveSource());
    toast.success(result.value === "" ? "Base URL cleared" : sessionOnly ? "Base URL saved (session only)" : "Base URL saved");
  };

  const handleReset = () => {
    setError(null);
    clearBaseUrl();
    const next = resolveBaseUrl();
    reconfigureSdk(next);
    setDraft(next);
    setEffective(next);
    setSource(resolveSource());
    setStorageWarning(false);
    toast.success("Reset to default");
  };

  const fullEffective = !effective || effective.startsWith("/") ? `${window.location.origin}${effective}` : effective;

  const copyUrl = async () => {
    try {
      await navigator.clipboard.writeText(fullEffective);
      toast.success("Copied");
    } catch {
      toast.error("Clipboard unavailable — copy blocked by the browser");
    }
  };

  const apiExample = withBase("/api/sessions");
  const sseExample = withBase("/api/events");

  return (
    <section aria-label="Settings" className="mx-auto flex max-w-3xl flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h1 className="flex items-center gap-2 text-lg font-semibold tracking-tight">
            <Settings className="h-4 w-4" aria-hidden="true" />
            Settings
          </h1>
          <p className="text-xs text-muted-foreground">
            Configure where the client sends API requests. Leave empty for same-origin (root). Applies immediately, no rebuild required.
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={() => setView("app")} aria-label="Close settings">
          Close
        </Button>
      </header>

      <Card>
        <CardHeader>
          <CardTitle>Server base URL</CardTitle>
          <CardDescription>
            Path like <code className="font-mono">/service-endpoint-wacalls</code> or a full <code className="font-mono">https://host</code> origin.
            Only enter origins you trust — requests including your API key are sent there.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSave} className="space-y-3">
            <div className="space-y-1">
              <Label htmlFor="wacalls-base-url">Base URL</Label>
              <Input
                id="wacalls-base-url"
                value={draft}
                onChange={(e) => {
                  setDraft(e.target.value);
                  setError(null);
                }}
                placeholder="/service-endpoint-wacalls"
                aria-describedby={error ? "wacalls-base-url-hint wacalls-base-url-error" : "wacalls-base-url-hint"}
                aria-invalid={error ? "true" : undefined}
              />
              <p id="wacalls-base-url-hint" className="text-xs text-muted-foreground">
                Empty = same-origin root. Non-ASCII hostnames must be punycode (xn--…). Used for REST (<code className="font-mono">{apiExample}</code>) and SSE (<code className="font-mono">{sseExample}</code>).
              </p>
              {error && (
                <p id="wacalls-base-url-error" role="alert" className="text-xs text-destructive">
                  {error}
                </p>
              )}
            </div>
            {storageWarning && (
              <p role="status" className="rounded-md border border-amber-500/50 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-400">
                Storage unavailable — setting applies to this session only.
              </p>
            )}
            <div className="flex flex-wrap gap-2">
              <Button type="submit">Save</Button>
              <Button type="button" variant="outline" onClick={handleReset}>
                Reset to default
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Effective base URL</CardTitle>
          <CardDescription>Resolved value and its source.</CardDescription>
        </CardHeader>
        <CardContent className="space-y-2 text-sm">
          <div className="flex items-center gap-2">
            <code className="rounded bg-muted px-2 py-1 font-mono text-xs">{effective || "(same-origin root)"}</code>
            <span className="text-xs text-muted-foreground">source: {source}</span>
          </div>
          <div className="flex items-center gap-2">
            <code className="rounded bg-muted px-2 py-1 font-mono text-xs">{fullEffective}</code>
            <Button variant="ghost" size="sm" onClick={() => void copyUrl()} aria-label="Copy WACALLS_SERVER_URL">
              <Copy className="h-4 w-4" aria-hidden="true" />
              Copy WACALLS_SERVER_URL
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            Use this value for the Chatwoot plugin <code className="font-mono">WACALLS_SERVER_URL</code>.
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Dev note</CardTitle>
          <CardDescription>
            In dev (<code className="font-mono">npm run dev</code>) the Vite proxy serves <code className="font-mono">/api</code> at the root. Leave the base empty there; a custom base targets a different prefix and needs a matching proxy rule.
          </CardDescription>
        </CardHeader>
      </Card>
    </section>
  );
};
