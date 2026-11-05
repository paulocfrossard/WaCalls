import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { SdkRequestError } from "wacalls-sdk";
import { sdkClient, sdkErrorCode } from "@/lib/sdk-client";
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
        // Whitelist-drop: only a known terminal code removes the ringing
        // entry. operator_busy (and any unparsable/unknown body) keeps it —
        // a stale entry self-heals via the owner-scoped call-list prune,
        // while a wrongly dropped ring can never come back.
        const code = sdkErrorCode(e);
        if (code === "call_claimed" || code === "not_acceptable") {
          removeIncoming(vars.sid, vars.callId);
          return;
        }
        toast.error(code === "operator_busy" ? "You are already on a call" : e.message);
        return;
      }
      toast.error(e.message);
    },
  });
