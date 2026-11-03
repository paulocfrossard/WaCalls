import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiGet, apiPost, apiDelete } from "./api";

describe("api wrappers use withBase", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: async () => "",
      json: async () => ({}),
    }));
  });
  afterEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
    vi.unstubAllEnvs();
  });

  it("default: fetch called with exactly /api/sessions", async () => {
    await apiGet("/api/sessions");
    const mock = (globalThis.fetch as ReturnType<typeof vi.fn>);
    expect(mock).toHaveBeenCalledWith("/api/sessions", expect.anything());
  });

  it("with configured base: fetch called with prefixed path", async () => {
    localStorage.setItem("wacalls.base_url", "/service-endpoint-wacalls");
    await apiGet("/api/sessions");
    const mock = (globalThis.fetch as ReturnType<typeof vi.fn>);
    expect(mock).toHaveBeenCalledWith("/service-endpoint-wacalls/api/sessions", expect.anything());
  });

  it("apiPost prefixes", async () => {
    localStorage.setItem("wacalls.base_url", "/p");
    await apiPost("/api/sessions", { name: "a" });
    const mock = (globalThis.fetch as ReturnType<typeof vi.fn>);
    expect(mock).toHaveBeenCalledWith("/p/api/sessions", expect.anything());
  });

  it("apiDelete prefixes", async () => {
    localStorage.setItem("wacalls.base_url", "/p");
    await apiDelete("/api/sessions/1");
    const mock = (globalThis.fetch as ReturnType<typeof vi.fn>);
    expect(mock).toHaveBeenCalledWith("/p/api/sessions/1", expect.anything());
  });
});
