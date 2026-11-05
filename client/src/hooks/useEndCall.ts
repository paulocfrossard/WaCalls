import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { SdkRequestError } from "wacalls-sdk";
import { sdkClient } from "@/lib/sdk-client";
import { notifySdkCallChanged } from "@/hooks/useSdkCall";

export const useEndCall = () =>
  useMutation({
    mutationFn: async (vars: { sid: string; callId: string }) => {
      // SDK: DELETE the call, then closes local media for it.
      try {
        await sdkClient.hangup(vars.sid, vars.callId);
      } catch (err) {
        // 404/409 are expected (call already gone / owned by someone else);
        // anything else (network, 5xx) means the server call may still be up.
        if (!(err instanceof SdkRequestError && (err.status === 404 || err.status === 409))) {
          toast.error(err instanceof Error ? err.message : String(err));
        }
      }
      finally {
        // The SDK tears down local media even when DELETE fails, and SSE
        // call:ended may be delayed/missing during reconnect — notify so the
        // meters/conn UI clears regardless.
        notifySdkCallChanged();
      }
    },
  });
