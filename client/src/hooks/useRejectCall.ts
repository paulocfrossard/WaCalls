import { useMutation } from "@tanstack/react-query";
import { sdkClient } from "@/lib/sdk-client";
import { clearIncoming } from "@/stores/calls";

export const useRejectCall = () =>
  useMutation({
    mutationFn: async (vars: { sid: string; callId: string }) => {
      try {
        await sdkClient.rejectCall(vars.sid, vars.callId);
      } catch {}
    },
    onSettled: () => clearIncoming(),
  });
