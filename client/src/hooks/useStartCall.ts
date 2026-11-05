import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { SdkRequestError } from "wacalls-sdk";
import { sdkClient } from "@/lib/sdk-client";
import { notifySdkCallChanged } from "@/hooks/useSdkCall";

export const useStartCall = (sid: string) =>
  useMutation({
    mutationFn: async (vars: { phone: string; record: boolean }) => {
      // SDK: POST /calls + open media (WebRTC/PCM) in one user-gesture-safe step.
      await sdkClient.startCall(sid, vars.phone, { record: vars.record });
      notifySdkCallChanged();
    },
    onError: (e: Error) => {
      // Classify on the HTTP status (the raw message embeds the whole
      // "METHOD path status body" string, so substring matching is brittle).
      if (e instanceof SdkRequestError) {
        if (e.status === 409) {
          toast.error("You are already on a call");
          return;
        }
        if (e.status === 429) {
          toast.error(
            e.body.includes("max total")
              ? "Server call limit reached."
              : "Limit reached: max concurrent calls.",
          );
          return;
        }
        if (e.status === 503) {
          toast.error("WhatsApp not paired.");
          return;
        }
      }
      toast.error(e.message);
    },
  });
