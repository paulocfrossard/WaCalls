export const SAMPLE_RATE = 16000;
export const PCM_CHANNEL_LABEL = "pcm";
export const PLC_ENABLED = true;

// field A/B: append ?aec=0 / ?ns=0 / ?agc=0 to disable echoCancellation /
// noiseSuppression / autoGainControl for one call (diagnosis only — never a
// fix). Absent (or any other value) = current behavior: all enabled.
const urlFlag = (name: string): boolean =>
  typeof window === "undefined" ? true : new URLSearchParams(window.location.search).get(name) !== "0";
export const AEC_ENABLED = urlFlag("aec");
export const NS_ENABLED = urlFlag("ns");
export const AGC_ENABLED = urlFlag("agc");
