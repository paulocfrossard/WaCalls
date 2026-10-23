import { defineConfig } from "tsup";

export default defineConfig({
  entry: ["src/index.ts"],
  format: ["esm", "iife"],
  globalName: "WaCallsSDK",
  dts: true,
  clean: true,
  sourcemap: true,
  target: "es2020",
  minify: true,
  // Worklets are embedded as template-literal strings in
  // src/worklets/sources.ts (strategy b) — no server-path fetches, identical
  // embedding in both the ESM and IIFE bundles.
});
