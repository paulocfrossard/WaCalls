import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
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
      const m = e.message;
      if (m.includes("429")) toast.error("Limit reached: max concurrent calls.");
      else if (m.includes("503")) toast.error("WhatsApp not paired.");
      else toast.error(m);
    },
  });
