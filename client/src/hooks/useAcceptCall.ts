import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { SdkRequestError } from "wacalls-sdk";
import { sdkClient } from "@/lib/sdk-client";
import { notifySdkCallChanged } from "@/hooks/useSdkCall";
import { removeIncoming } from "@/stores/calls";

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
      removeIncoming(vars.sid, vars.callId);
    },
    onError: (e: Error, vars: { sid: string; callId: string }) => {
      if (e instanceof SdkRequestError && e.status === 409) {
        // The server distinguishes three 409s. Only "operator already on a
        // call" leaves the ringing call unclaimed — keep it queued so the
        // operator can retry after ending the current call. The other two
        // (claimed by another client / not in an acceptable state) mean this
        // operator can no longer act on the call: drop it from the queue.
        if (e.body.includes("operator already on a call")) {
          toast.error("You are already on a call");
          return;
        }
        removeIncoming(vars.sid, vars.callId);
        return;
      }
      toast.error(e.message);
    },
  });
