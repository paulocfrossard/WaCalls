const RING_SIZE = 16000 * 2;
const HISTORY_SIZE = 128; // ~8 ms @ 16 kHz — janela de concealment (crossfade)
const FADE_SAMPLES = 128; // fade-out linear de ~8 ms (na faixa 5–10 ms pedida)
const MAX_CONCEAL_QUANTA = 2; // limite rígido: 2 quanta = 16 ms @ 16 kHz
const MAX_CONCEAL_SAMPLES = MAX_CONCEAL_QUANTA * 128;
const STATS_INTERVAL_QUANTA = 125; // ~1 s @ 16 kHz / 128 samples por quantum

class PlaybackProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();
    // kill-switch: ?plc=0 (main thread) → legacy no-PLC path, default ON
    const opts = options ?? this.options;
    this.plcEnabled = opts?.processorOptions?.plcEnabled !== false;
    this.ring = new Float32Array(RING_SIZE);
    this.read = 0;
    this.write = 0;
    this.available = 0;

    // Histórico circular dos últimos samples lidos (para PLC).
    this.history = new Float32Array(HISTORY_SIZE);
    this.historyWrite = 0; // próxima posição de escrita no histórico
    this.historyLen = 0; // samples válidos (≤ HISTORY_SIZE)
    this.concealPos = 0; // samples já concealidos no episódio atual

    // Contadores para A/B.
    this.drops = 0; // samples descartados por overwrite-oldest no ring
    this.underruns = 0; // quanta com ≥1 sample em underrun
    this.concealed = 0; // quanta em que PLC produziu áudio (fade)
    this.silenced = 0; // quanta de underrun que saíram como silêncio seco
    this.quantaSinceStats = 0;

    this.port.onmessage = (e) => {
      const data = e.data;
      // Poll sob demanda: main thread envia a string "stats".
      if (data === "stats") {
        this.postStats();
        return;
      }
      for (let i = 0; i < data.length; i += 1) {
        this.ring[this.write] = data[i];
        this.write = (this.write + 1) % RING_SIZE;
        if (this.available < RING_SIZE) {
          this.available += 1;
        } else {
          this.read = (this.read + 1) % RING_SIZE;
          this.drops += 1;
        }
      }
    };
  }

  postStats() {
    this.port.postMessage({
      type: "stats",
      drops: this.drops,
      underruns: this.underruns,
      concealed: this.concealed,
      silenced: this.silenced,
    });
  }

  process(_inputs, outputs) {
    const out = outputs[0][0];
    if (!out) return true;
    let hadUnderrun = false;
    let hadConcealed = false;
    let hadSilence = false;
    for (let i = 0; i < out.length; i += 1) {
      if (this.available > 0) {
        const sample = this.ring[this.read];
        this.read = (this.read + 1) % RING_SIZE;
        this.available -= 1;
        out[i] = sample;
        if (this.plcEnabled) {
          // Alimenta o histórico e encerra qualquer episódio de underrun.
          this.history[this.historyWrite] = sample;
          this.historyWrite = (this.historyWrite + 1) % HISTORY_SIZE;
          if (this.historyLen < HISTORY_SIZE) this.historyLen += 1;
          this.concealPos = 0;
        }
      } else {
        hadUnderrun = true;
        if (!this.plcEnabled) {
          // kill-switch: legacy path — silêncio seco imediato, sem history/PLC
          // (contadores underruns/silenced continuam contando).
          out[i] = 0;
          hadSilence = true;
        } else {
          const idx = this.concealPos;
          if (idx < MAX_CONCEAL_SAMPLES && this.historyLen > 0) {
            // PLC: repete os últimos samples lidos com fade-out linear até 0.
            const gain = idx < FADE_SAMPLES ? 1 - idx / FADE_SAMPLES : 0;
            if (idx < this.historyLen && gain > 0) {
              const histIdx = (this.historyWrite + idx) % HISTORY_SIZE;
              out[i] = this.history[histIdx] * gain;
              hadConcealed = true;
            } else {
              out[i] = 0;
              hadSilence = true;
            }
          } else {
            // Silêncio seco após o limite rígido de concealment (ou sem histórico).
            out[i] = 0;
            hadSilence = true;
          }
          this.concealPos += 1;
        }
      }
    }
    if (hadUnderrun) this.underruns += 1;
    if (hadConcealed) this.concealed += 1;
    if (hadSilence) this.silenced += 1;

    this.quantaSinceStats += 1;
    if (this.quantaSinceStats >= STATS_INTERVAL_QUANTA) {
      this.quantaSinceStats = 0;
      this.postStats();
    }
    return true;
  }
}

registerProcessor("playback-processor", PlaybackProcessor);
