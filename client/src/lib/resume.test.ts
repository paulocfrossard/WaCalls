import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { hasLegClaim, needsAudioResume } from "./resume";

// sessionStorage is per-tab in the browser; jsdom provides it globally, so
// tests seed keys directly with the same prefix the SDK writes.
describe("hasLegClaim", () => {
  beforeEach(() => {
    sessionStorage.clear();
  });
  afterEach(() => {
    sessionStorage.clear();
  });

  it("returns false when no claim was written", () => {
    expect(hasLegClaim("call-1")).toBe(false);
  });

  it("returns true when this tab holds the claim", () => {
    sessionStorage.setItem("wacalls.leg.call-1", "1");
    expect(hasLegClaim("call-1")).toBe(true);
  });

  it("is scoped per callId", () => {
    sessionStorage.setItem("wacalls.leg.call-1", "1");
    expect(hasLegClaim("call-2")).toBe(false);
  });

  it("ignores values other than the SDK's '1'", () => {
    sessionStorage.setItem("wacalls.leg.call-1", "0");
    expect(hasLegClaim("call-1")).toBe(false);
  });

  it("returns false when sessionStorage is unavailable (non-browser env)", () => {
    const original = Object.getOwnPropertyDescriptor(globalThis, "sessionStorage");
    Object.defineProperty(globalThis, "sessionStorage", {
      value: undefined,
      configurable: true,
      writable: true,
    });
    try {
      expect(hasLegClaim("call-1")).toBe(false);
    } finally {
      if (original) Object.defineProperty(globalThis, "sessionStorage", original);
      else delete (globalThis as { sessionStorage?: Storage }).sessionStorage;
    }
  });
});

describe("needsAudioResume", () => {
  it("requires the leg claim: never resumable without it (M1)", () => {
    expect(needsAudioResume("connected", false, false)).toBe(false);
    expect(needsAudioResume("starting", false, false)).toBe(false);
    expect(needsAudioResume("ringing", false, false)).toBe(false);
  });

  it("resumes a connected call with claim but no local connection", () => {
    expect(needsAudioResume("connected", false, true)).toBe(true);
  });

  it("does not resume while a local connection exists", () => {
    expect(needsAudioResume("connected", true, true)).toBe(false);
    expect(needsAudioResume("ringing", true, true)).toBe(false);
  });

  it("never resumes an ended call, even with claim and no connection", () => {
    expect(needsAudioResume("ended", false, true)).toBe(false);
    expect(needsAudioResume("ended", true, true)).toBe(false);
  });

  it("M3: a setup-phase call (starting/ringing) WITH claim is resumable", () => {
    expect(needsAudioResume("starting", false, true)).toBe(true);
    expect(needsAudioResume("ringing", false, true)).toBe(true);
  });
});
