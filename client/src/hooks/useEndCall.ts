import { useMutation } from "@tanstack/react-query";
import { sdkClient } from "@/lib/sdk-client";
import { notifySdkCallChanged } from "@/hooks/useSdkCall";

export const useEndCall = () =>
  useMutation({
    mutationFn: async (vars: { sid: string; callId: string }) => {
      // SDK: DELETE the call, then closes local media for it.
      try {
        await sdkClient.hangup(vars.sid, vars.callId);
      } catch {}
      finally {
        // The SDK tears down local media even when DELETE fails, and SSE
        // call:ended may be delayed/missing during reconnect — notify so the
        // meters/conn UI clears regardless.
        notifySdkCallChanged();
      }
    },
  });
