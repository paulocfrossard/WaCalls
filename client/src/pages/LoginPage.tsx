import { useState, type FormEvent } from "react";
import { Loader2, PhoneCall } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/stores/auth";

type LoginResponse = {
  expiresAt: number;
  user: { id: string; username: string; role: string };
};

/** Backend errors are `{"error": "..."}` — fall back to the HTTP status. */
const readError = async (r: Response): Promise<string> => {
  try {
    const body: unknown = await r.json();
    if (
      typeof body === "object" &&
      body !== null &&
      "error" in body &&
      typeof (body as { error: unknown }).error === "string"
    ) {
      return (body as { error: string }).error;
    }
  } catch {
    // Not JSON (proxy/HTML error page): use the status below.
  }
  if (r.status === 429) return "Too many attempts — try again in a minute.";
  return `Login failed (${r.status})`;
};

/**
 * Authentication gate (no router): the form posts directly (NOT through
 * apiFetch, so a 401 here renders an error instead of redirecting), stores the
 * session via useAuth and lets App swap this page for the app shell.
 */
export const LoginPage = () => {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (submitting) return; // guard double submit (Enter + click)
    const name = username.trim();
    if (!name || !password) {
      const message = "Enter your username and password.";
      setError(message);
      toast.error(message);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const r = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ username: name, password }),
      });
      if (!r.ok) throw new Error(await readError(r));
      const data = (await r.json()) as LoginResponse;
      if (!data?.user?.username) throw new Error("Login response is missing the user.");
      const expiresAt = typeof data.expiresAt === "number" && Number.isFinite(data.expiresAt) ? data.expiresAt : null;
      // Cookie-only session: the backend sets an HttpOnly cookie and returns no
      // token, so the in-memory token stays null and requests rely on the cookie.
      useAuth.getState().login(data.user.username, null, expiresAt);
      setPassword(""); // never keep the credential in component state
    } catch (err) {
      const message = err instanceof Error ? err.message : "Login failed";
      setError(message);
      toast.error(message);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <main className="flex min-h-screen items-center justify-center bg-background px-4 py-10">
      <div className="w-full max-w-sm space-y-6">
        <div className="flex items-center justify-center gap-2">
          <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <PhoneCall className="h-4 w-4" aria-hidden="true" />
          </span>
          <span className="text-xl font-semibold tracking-tight">WaCalls</span>
        </div>

        <Card>
          <CardHeader>
            <CardTitle>Sign in</CardTitle>
            <CardDescription>Use your WaCalls account to manage sessions and calls.</CardDescription>
          </CardHeader>
          <CardContent>
            <form onSubmit={(e) => void onSubmit(e)} className="space-y-4">
              <div className="space-y-1.5">
                <Label htmlFor="login-username">Username</Label>
                <Input
                  id="login-username"
                  name="username"
                  type="text"
                  autoComplete="username"
                  autoFocus
                  required
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  disabled={submitting}
                  aria-invalid={error !== null}
                  aria-describedby={error !== null ? "login-error" : undefined}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="login-password">Password</Label>
                <Input
                  id="login-password"
                  name="password"
                  type="password"
                  autoComplete="current-password"
                  required
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  disabled={submitting}
                  aria-invalid={error !== null}
                  aria-describedby={error !== null ? "login-error" : undefined}
                />
              </div>
              {error !== null && (
                <p id="login-error" role="alert" className="text-sm text-destructive">
                  {error}
                </p>
              )}
              <Button type="submit" className="w-full" disabled={submitting}>
                {submitting && <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />}
                {submitting ? "Signing in…" : "Sign in"}
              </Button>
            </form>
          </CardContent>
        </Card>
      </div>
    </main>
  );
};
