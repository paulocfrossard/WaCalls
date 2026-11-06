import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { sdkClient } from "@/lib/sdk-client";
import { notifySdkCallChanged } from "@/hooks/useSdkCall";

// useResumeCall re-attaches the browser audio leg to a call that is still live
// on the server (held open by the grace window after a refresh). Unlike
// start/accept it never ends the call on failure: the call keeps running
// server-side and the operator can retry.
export const useResumeCall = () =>
  useMutation({
    mutationFn: async (vars: { sid: string; callId: string }) => {
      await sdkClient.resumeCall(vars.sid, vars.callId);
      // #active was set inside the SDK; tell React to re-read the connection.
      notifySdkCallChanged();
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err)),
  });
