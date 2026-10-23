/**
 * PCM helpers — pure Float32 <-> Int16 little-endian conversion.
 * Zero imports; only Web APIs / ES builtins. Port of client/src/lib/pcm.ts.
 */

export const float32ToInt16LE = (pcm: Float32Array): ArrayBuffer => {
  // Int16Array view: one allocation + indexed writes — no per-sample
  // DataView.setInt16, no Math.round. (Host endianness: every browser with
  // WebAudio is little-endian, matching the wire format.)
  const out = new Int16Array(pcm.length);
  for (let i = 0; i < pcm.length; i += 1) {
    let s = pcm[i];
    if (Number.isNaN(s)) s = 0;
    else if (s > 1) s = 1;
    else if (s < -1) s = -1;
    if (s < 0) {
      // Negative branch: v = s * 32768 (exact power-of-two scale). The
      // original was Math.round(v) = floor(v + 0.5); trunc(v - 0.5) matches
      // it byte-for-byte EXCEPT at exact halves (v = n + 0.5), where
      // Math.round goes toward +inf — corrected by the `r === u` branch.
      const u = s * 32768 - 0.5;
      let r = u | 0;
      if (r === u) r += 1;
      out[i] = r;
    } else {
      // Non-negative branch: v = s * 32767 ≥ 0 → trunc(v + 0.5) === floor
      // (v + 0.5) === Math.round(v).
      out[i] = (s * 32767 + 0.5) | 0;
    }
  }
  return out.buffer;
};

export const int16LEToFloat32 = (buf: ArrayBuffer): Float32Array => {
  const view = new DataView(buf);
  const n = Math.floor(buf.byteLength / 2);
  const out = new Float32Array(n);
  for (let i = 0; i < n; i += 1) out[i] = view.getInt16(i * 2, true) / 32768;
  return out;
};
