import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  clearBaseUrl,
  normalizeBaseUrl,
  persistBaseUrl,
  resolveBaseUrl,
  withBase,
} from "./base";

describe("normalizeBaseUrl", () => {
  it("empty → empty", () => {
    expect(normalizeBaseUrl("")).toEqual({ ok: true, value: "" });
    expect(normalizeBaseUrl("   ")).toEqual({ ok: true, value: "" });
  });
  it("strips trailing slashes on paths", () => {
    expect(normalizeBaseUrl("/service-endpoint-wacalls/")).toEqual({ ok: true, value: "/service-endpoint-wacalls" });
    expect(normalizeBaseUrl("/a//")).toEqual({ ok: true, value: "/a" });
  });
  it("rejects protocol-relative", () => {
    expect(normalizeBaseUrl("//evil.com")).toMatchObject({ ok: false });
  });
  it("rejects javascript:/data:", () => {
    expect(normalizeBaseUrl("javascript:alert(1)")).toMatchObject({ ok: false });
    expect(normalizeBaseUrl("data:text/html,x")).toMatchObject({ ok: false });
  });
  it("rejects query/hash/whitespace", () => {
    expect(normalizeBaseUrl("/a?x=1")).toMatchObject({ ok: false });
    expect(normalizeBaseUrl("/a#frag")).toMatchObject({ ok: false });
    expect(normalizeBaseUrl("/a b")).toMatchObject({ ok: false });
  });
  it("accepts absolute https origin", () => {
    expect(normalizeBaseUrl("https://chatwoot.caeddigital.net/service-endpoint-wacalls/")).toEqual({
      ok: true,
      value: "https://chatwoot.caeddigital.net/service-endpoint-wacalls",
    });
  });
  it("rejects http on https page", () => {
    expect(normalizeBaseUrl("http://example.com/x", "https:")).toMatchObject({ ok: false });
    expect(normalizeBaseUrl("http://example.com/x", "http:")).toEqual({ ok: true, value: "http://example.com/x" });
  });
  it("rejects garbage", () => {
    expect(normalizeBaseUrl("not-a-url")).toMatchObject({ ok: false });
  });
  it("idempotent", () => {
    const v = "/service-endpoint-wacalls";
    const r = normalizeBaseUrl(v);
    if (r.ok) expect(normalizeBaseUrl(r.value)).toEqual(r);
  });
});

describe("resolveBaseUrl precedence", () => {
  beforeEach(() => {
    clearBaseUrl();
    localStorage.clear();
    vi.unstubAllEnvs();
  });
  afterEach(() => {
    clearBaseUrl();
    localStorage.clear();
    vi.unstubAllEnvs();
  });

  it("fallback to empty when nothing set", () => {
    expect(resolveBaseUrl()).toBe("");
  });

  it("env wins over empty default", () => {
    vi.stubEnv("VITE_WACALLS_BASE_URL", "/from-env");
    expect(resolveBaseUrl()).toBe("/from-env");
  });

  it("localStorage wins over env", () => {
    vi.stubEnv("VITE_WACALLS_BASE_URL", "/from-env");
    persistBaseUrl("/from-panel");
    expect(resolveBaseUrl()).toBe("/from-panel");
  });

  it("storage throw falls through to env", () => {
    vi.stubEnv("VITE_WACALLS_BASE_URL", "/from-env");
    const spy = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("disabled");
    });
    expect(resolveBaseUrl()).toBe("/from-env");
    spy.mockRestore();
  });
});

describe("withBase identity", () => {
  beforeEach(() => { clearBaseUrl(); localStorage.clear(); vi.unstubAllEnvs(); });
  afterEach(() => { clearBaseUrl(); localStorage.clear(); vi.unstubAllEnvs(); });

  it("identity when base is empty", () => {
    expect(withBase("/api/sessions")).toBe("/api/sessions");
    expect(withBase("/api/debug/logs?since=1")).toBe("/api/debug/logs?since=1");
  });

  it("prefixes when base set", () => {
    persistBaseUrl("/service-endpoint-wacalls");
    expect(withBase("/api/sessions")).toBe("/service-endpoint-wacalls/api/sessions");
  });

  it("prefixes with absolute base", () => {
    persistBaseUrl("https://api.example.com/v1");
    expect(withBase("/api/sessions")).toBe("https://api.example.com/v1/api/sessions");
    expect(withBase("/api/sessions")).not.toBe("https://api.example.com/api/sessions");
  });
});

describe("persistBaseUrl / clearBaseUrl", () => {
  beforeEach(() => { clearBaseUrl(); localStorage.clear(); vi.unstubAllEnvs(); });
  afterEach(() => { clearBaseUrl(); localStorage.clear(); vi.unstubAllEnvs(); });

  it("persist + clear roundtrip", () => {
    expect(persistBaseUrl("/x/")).toEqual({ ok: true });
    expect(localStorage.getItem("wacalls.base_url")).toBe("/x");
    clearBaseUrl();
    expect(localStorage.getItem("wacalls.base_url")).toBeNull();
  });

  it("invalid input returns validation", () => {
    expect(persistBaseUrl("javascript:alert(1)")).toEqual({ ok: false, reason: "validation" });
  });

  it("storage failure -> sessionOnly + precedence + recovery", () => {
    const spy = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("full"); });
    expect(persistBaseUrl("/x")).toEqual({ ok: true, sessionOnly: true });
    expect(resolveBaseUrl()).toBe("/x");
    clearBaseUrl();
    expect(resolveBaseUrl()).toBe("");
    spy.mockRestore();
  });
});
