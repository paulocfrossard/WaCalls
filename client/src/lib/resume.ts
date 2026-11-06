import type { CallStatus } from "@/types/call";

// Per-tab leg claim key prefix — written by the SDK (wacalls-sdk) into
// sessionStorage when it opens the WebRTC leg for a call, and cleared when
// that leg tears down. sessionStorage is per-tab, so the claim answers
// exactly one question: "did THIS tab hold the leg for this call?"
const LEG_CLAIM_PREFIX = "wacalls.leg.";

// hasLegClaim reports whether this tab holds the per-tab leg claim for the
// given call. Guarded for non-browser/test environments (and for storage
// that throws, e.g. sandboxed iframes) — always false there, so resume is
// never attempted without a real claim.
export const hasLegClaim = (callId: string): boolean => {
  try {
    if (typeof sessionStorage === "undefined") return false;
    return sessionStorage[LEG_CLAIM_PREFIX + callId] === "1";
  } catch {
    return false;
  }
};

// needsAudioResume reports whether an owned call is live on the server but has
// no local audio leg IN THIS TAB, which happens after a page refresh drops the
// WebRTC bridge. Three conditions, all required:
//  - hasLegClaim: only the tab that held the leg may re-attach it — a second
//    tab of the same profile must never steal the call (M1). The claim is also
//    what makes a call still in setup (starting/ringing) resumable after a
//    refresh (M3): it distinguishes "this tab held the leg" from "another
//    tab's call", so no status gating on "connected" is needed;
//  - !hasConnection: this tab has no local leg right now;
//  - status !== "ended": a finished call is never resumable.
export const needsAudioResume = (
  status: CallStatus,
  hasConnection: boolean,
  hasLegClaim: boolean,
): boolean => hasLegClaim && !hasConnection && status !== "ended";
