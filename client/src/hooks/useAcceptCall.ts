import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { SdkRequestError } from "wacalls-sdk";
import { sdkClient } from "@/lib/sdk-client";
import { notifySdkCallChanged } from "@/hooks/useSdkCall";
import { clearIncoming } from "@/stores/calls";

export const useAcceptCall = () =>
  useMutation({
    mutationFn: async (vars: { sid: string; callId: string }) => {
      try {
        // SDK: POST /accept + open media in one step.
        await sdkClient.answerCall(vars.sid, vars.callId);
      } catch (err) {
        // Compensate ONLY for media-phase failures: the accept succeeded
        // server-side but opening WebRTC failed — hang up to undo our own
        // half-open accept. Never hang up on an accept-phase REST error:
        //   - 409 = the call was already claimed by another owner (never undo);
        //   - other /accept failures mean it was never accepted (nothing to undo).
        const isAcceptPhase =
          err instanceof SdkRequestError && err.path.endsWith("/accept");
        if (!isAcceptPhase) {
          try {
            await sdkClient.hangup(vars.sid, vars.callId);
          } catch {}
        }
        throw err;
      }
      notifySdkCallChanged();
      clearIncoming();
    },
    onError: (e: Error) => {
      if (e instanceof SdkRequestError && e.status === 409) {
        clearIncoming();
        return;
      }
      toast.error(e.message);
    },
  });
