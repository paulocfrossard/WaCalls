import { useEffect, useState } from "react";
import { Loader2 } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { useSessions } from "@/stores/sessions";
import type { SessionInfo } from "@/types/session";

export const SessionPairing = ({ session }: { session: SessionInfo }) => {
  const qr = useSessions((s) => s.qrs[session.id]);
  const [timedOut, setTimedOut] = useState(false);

  useEffect(() => {
    if (qr) {
      setTimedOut(false);
      return;
    }
    const t = window.setTimeout(() => setTimedOut(true), 30_000);
    return () => window.clearTimeout(t);
  }, [qr]);

  return (
    <div className="flex min-h-[55vh] items-center justify-center">
      <Card className="w-full max-w-md">
        <CardHeader className="items-center text-center">
          <CardTitle>Pair {session.name}</CardTitle>
          <CardDescription>
            Open WhatsApp → Linked devices → Link a device, then scan.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col items-center gap-4">
          {qr ? (
            <div className="rounded-lg border bg-white p-3">
              <QRCodeSVG value={qr} size={232} marginSize={1} />
            </div>
          ) : session.state === "logged_out" ? (
            <Badge variant="destructive">Disconnected — use Reactivate above to get a QR</Badge>
          ) : timedOut ? (
            <Badge variant="muted" className="gap-1.5">
              No QR yet — use Reactivate above to retry.
            </Badge>
          ) : (
            <>
              <Skeleton className="h-[258px] w-[258px] rounded-lg" />
              <Badge variant="muted" className="gap-1.5">
                <Loader2 className="h-3 w-3 animate-spin" /> Waiting for QR…
              </Badge>
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
};
