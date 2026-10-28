import { useState, type ReactNode } from "react";
import { LogOut, Menu, PhoneCall } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { sdkClient } from "@/lib/sdk-client";
import { getToken, useAuth } from "@/stores/auth";
import { Sidebar } from "./Sidebar";
import { ThemeToggle } from "./ThemeToggle";

export const AppShell = ({ children }: { children: ReactNode }) => {
  const [mobileOpen, setMobileOpen] = useState(false);

  const signOut = async () => {
    // End the active call while the token is still valid (DELETE needs it).
    await sdkClient.hangupActive().catch(() => {});
    // Capture the token before logout() clears it: the HttpOnly cookie is not
    // available when -cookie-secure=true runs over plain HTTP, so the bearer
    // header is the only way the server can identify and revoke the session.
    const token = getToken();
    useAuth.getState().logout();
    // Server-side revoke is best effort and must not block the UI.
    void fetch("/api/auth/logout", {
      method: "POST",
      headers: token ? { Authorization: `Bearer ${token}` } : {},
      credentials: "include",
      signal: AbortSignal.timeout(3000),
    }).catch(() => {});
  };

  return (
    <div className="flex min-h-screen flex-col">
      <header className="sticky top-0 z-30 flex items-center justify-between border-b bg-background/80 px-4 py-3 backdrop-blur sm:px-6">
        <div className="flex items-center gap-2">
          <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
            <SheetTrigger asChild>
              <Button variant="outline" size="icon" className="md:hidden" aria-label="Accounts">
                <Menu className="h-4 w-4" />
              </Button>
            </SheetTrigger>
            <SheetContent side="left" className="w-72 p-0">
              <SheetTitle className="px-3 pt-3">Accounts</SheetTitle>
              <Sidebar onNavigate={() => setMobileOpen(false)} />
            </SheetContent>
          </Sheet>
          <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <PhoneCall className="h-4 w-4" />
          </span>
          <span className="text-lg font-semibold tracking-tight">WaCalls</span>
        </div>
        <div className="flex items-center gap-2">
          <ThemeToggle />
          <Button variant="ghost" size="icon" onClick={() => void signOut()} aria-label="Sign out" title="Sign out">
            <LogOut className="h-4 w-4" aria-hidden="true" />
          </Button>
        </div>
      </header>
      <div className="flex flex-1">
        <aside className="hidden w-64 shrink-0 border-r md:block">
          <Sidebar />
        </aside>
        <main className="flex-1 px-4 py-6 sm:px-6">{children}</main>
      </div>
    </div>
  );
};
