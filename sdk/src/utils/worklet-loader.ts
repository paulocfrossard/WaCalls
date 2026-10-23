/**
 * AudioWorklet module loader — installs embedded processor source code into
 * an AudioContext without any server-path fetch.
 *
 * The worklet code (a string) is turned into a Blob, handed to
 * `audioWorklet.addModule()` via an object URL, and the object URL is
 * revoked as soon as addModule settles (success or failure — no leaks).
 */
export const addWorkletModule = async (ctx: AudioContext, code: string): Promise<void> => {
  const blob = new Blob([code], { type: "application/javascript" });
  const url = URL.createObjectURL(blob);
  try {
    await ctx.audioWorklet.addModule(url);
  } finally {
    URL.revokeObjectURL(url);
  }
};
