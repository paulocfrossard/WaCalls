import { useEffect, useState } from "react";
import { AEC_ENABLED, AGC_ENABLED, NS_ENABLED } from "@/constants/audio";

export type AudioDevice = { deviceId: string; label: string };

export const useAudioDevices = () => {
  const [mics, setMics] = useState<AudioDevice[]>([]);
  const [outs, setOuts] = useState<AudioDevice[]>([]);

  useEffect(() => {
    (async () => {
      try {
        (
          await navigator.mediaDevices.getUserMedia({
            audio: {
              // field A/B: same ?aec=0/?ns=0/?agc=0 flags as the call gUM
              echoCancellation: AEC_ENABLED,
              noiseSuppression: NS_ENABLED,
              autoGainControl: AGC_ENABLED,
            },
          })
        )
          .getTracks()
          .forEach((t) => t.stop());
      } catch {}
      const list = await navigator.mediaDevices.enumerateDevices();
      setMics(
        list
          .filter((d) => d.kind === "audioinput")
          .map((d) => ({ deviceId: d.deviceId, label: d.label || "Default mic" })),
      );
      setOuts(
        list
          .filter((d) => d.kind === "audiooutput")
          .map((d) => ({ deviceId: d.deviceId, label: d.label || "Default speaker" })),
      );
    })();
  }, []);

  return { mics, outs };
};
