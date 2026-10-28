import { useEffect, useState } from "react";
import { Loader2, PlusCircle } from "lucide-react";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Toaster } from "@/components/ui/sonner";
import { AppShell } from "@/components/layout/AppShell";
import { CallsPage } from "@/pages/CallsPage";
import { SessionPairing } from "@/components/domain/session/SessionPairing";
import { SessionHeader } from "@/components/domain/session/SessionHeader";
import { IncomingCallModal } from "@/components/domain/call/IncomingCallModal";
import { EmptyState } from "@/components/shared/EmptyState";
import { ensureSessionsWired, useSessions } from "@/stores/sessions";
import { ensureCallsWired } from "@/stores/calls";
import { resetAllWiring } from "@/lib/wiring";
import { useAuth, bootstrapAuth } from "@/stores/auth";
import { useTheme } from "@/stores/theme";
import { useUi } from "@/stores/ui";
import { DebugPage } from "@/pages/DebugPage";
import { LoginPage } from "@/pages/LoginPage";

export const App = () => {
  const isAuthenticated = useAuth((s) => s.isAuthenticated);
  const sessions = useSessions((s) => s.sessions);
  const activeId = useSessions((s) => s.activeId);
  const theme = useTheme((s) => s.theme);
  const view = useUi((s) => s.view);
  // Cookie sessions have no in-memory token at boot: probe /api/auth/me once
  // and keep a neutral loading state (never the login page) while it resolves.
  const [booting, setBooting] = useState(true);

  useEffect(() => {
    void bootstrapAuth().finally(() => setBooting(false));
  }, []);

  // Auth gate: wire the SDK/stores only while signed in. The effect owns the
  // teardown (resetAllWiring + disconnect) so no store module imports another
  // store's wiring (avoids cycles).
  useEffect(() => {
    if (booting) return;
    if (!isAuthenticated) {
      // End the active call BEFORE disconnect() (and before any caller clears
      // the token). The effect cannot be async itself, so await inside an IIFE.
      void (async () => {
        await resetAllWiring();
      })();
      return;
    }
    ensureSessionsWired();
    ensureCallsWired();
  }, [booting, isAuthenticated]);

  const active = sessions.find((s) => s.id === activeId) ?? null;

  if (booting) {
    return (
      <TooltipProvider delayDuration={200}>
        <main className="flex min-h-screen items-center justify-center bg-background">
          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" aria-hidden="true" />
          <span className="sr-only">Restoring session…</span>
        </main>
        <Toaster theme={theme} position="top-right" richColors closeButton />
      </TooltipProvider>
    );
  }

  return (
    <TooltipProvider delayDuration={200}>
      {isAuthenticated ? (
        <>
          <AppShell>
            {view === "debug" ? (
              <DebugPage />
            ) : sessions.length === 0 ? (
              <EmptyState
                icon={<PlusCircle className="h-6 w-6" />}
                title="No accounts yet"
                description="Create your first WhatsApp account from the sidebar to start calling."
              />
            ) : active ? (
              <div className="space-y-6">
                <SessionHeader session={active} />
                {active.state === "open" ? <CallsPage sid={active.id} /> : <SessionPairing session={active} />}
              </div>
            ) : (
              <EmptyState title="Select an account" description="Choose an account from the sidebar." />
            )}
          </AppShell>
          <IncomingCallModal />
        </>
      ) : (
        <LoginPage />
      )}
      <Toaster theme={theme} position="top-right" richColors closeButton />
    </TooltipProvider>
  );
};
