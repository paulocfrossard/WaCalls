import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { SdkRequestError } from "wacalls-sdk";
import { sdkClient } from "@/lib/sdk-client";
import { removeIncoming } from "@/stores/calls";

export const useRejectCall = () =>
  useMutation({
    mutationFn: (vars: { sid: string; callId: string }) =>
      sdkClient.rejectCall(vars.sid, vars.callId),
    onSuccess: (_data, vars) => removeIncoming(vars.callId),
    onError: (e: Error, vars) => {
      // 404/409: already gone or claimed by another operator — drop the entry.
      // Any other failure keeps it ringing so the operator can retry.
      if (e instanceof SdkRequestError && (e.status === 404 || e.status === 409)) {
        removeIncoming(vars.callId);
        return;
      }
      toast.error(e.message);
    },
  });
