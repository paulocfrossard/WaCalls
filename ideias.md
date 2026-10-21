Decision Gate
- Goal ✓ — Entender a E/S de áudio do we-calls e confrontá-la com as melhorias Go 2025/26 da nota, com claims fundamentadas via @citer
- Target ✓ — /home/usuario/git/we-calls (pure Go, sem cgo, MLow vendored, PCM 16 kHz over datachannel)
- Acceptance criteria ✓ — fluxo completo mic→alto-falante mapeado com path:line + claims externas verificadas (SUPPORTED/NOT FOUND)
STATE: pipeline mapeado nos dois lados; 3 libs da nota confirmadas como existentes; os números de latência/jitter da nota não têm fonte primária rastreável; gaps concretos identificados no código.
Skills Grounded
Mantidas da rodada anterior: plan-agent (.../plan-agent/SKILL.md:28), project-init (.../project-init/SKILL.md:24), opencode-workflow (.../opencode-workflow/SKILL.md:44), writing-plans (.../writing-plans/SKILL.md:26). Não há skill de áudio/DSP no registry.
Estado da E/S de áudio (fatos do código)
MIC (≈48k HW)
 │ getUserMedia SEM constraints AEC/NS/AGC          webrtc.ts:24-26
 │ AudioContext({sampleRate:16000})                  webrtc.ts:33, constants/audio.ts:1
 │ AudioWorklet capture: 128 samples (8 ms) passthrough  capture-processor.js:1-11
 │ Int16LE → 256 B → dc.send a cada 8 ms             pcm.ts:1-11, webrtc.ts:40-42
 │ DataChannel "pcm": ordered+reliable, SEM backpressure  webrtc.ts:30-31
 ▼
SERVIDOR (uplink)
 │ OnMessage → Int16→f32 (alloc/msg)                 bridge.go:42-46, pcm.go:16-23
 │ FeedCapturedPCM sob m.mu (tudo inline)             callmanager_media.go:22-55
 │ reframe p/ 960 samples (60 ms) → MLow encode       codec.go:24-25, mlow_codec_native.go:18
 │ RTP PT=120 @16k (+960 ts/pcto) → SRTP Protect      rtp.go:174-187, srtp.go:72-95
 │ relay.Broadcast SINCRONO/BLOQUEANTE                sctprelay.go:345-355
 ▼  (relé SRTP/SCTP do WhatsApp)
SERVIDOR (downlink)
 │ onRelayData → filtros → Unprotect (SÓ decrypt, SEM verificação HMAC!)  callmanager_media.go:146, srtp.go:97-122
 │ MLow decode → NormalizeFrame (só pad/truncate — NÃO há resampler)  resample.go:3-10
 │ WritePCM → dc.Send síncrono, erro descartado       session.go:90-96, bridge.go:79-85
 ▼
BROWSER (downlink)
 │ ring buffer FIXO de 2 s (32000 amostras), overwrite-oldest, underrun=zeros  playback-processor.js:1-33
 │ <audio srcObject> → setSinkId → alto-falante       CallCard.tsx:53-69
Ausências explícitas (grep zero): jitter buffer adaptativo/prebuffer/reorder/descarte de frame atrasado; LockOSThread/SetGCPercent; canais/filas no caminho de mídia; backpressure (bufferedAmount); metadados de frame (seq/ts no payload do DC); AEC/NS/AGC; resampler de taxa (todas as bordas são 16 kHz — o 48k→16k é delegado ao browser).
Achado de segurança além da nota: srtp.Unprotect decifra sem verificar a tag HMAC (computeAuthTag só é chamado de Protect, srtp.go:161-169) — pacotes forjados/deformados são aceitos se passarem os filtros de forma.
Nota da anotação vs. verificação do @citer
Claim da nota	Veredito
go-audio-resampler (libsoxr pure Go, Kaiser polifásico, SIMD AVX2/SSE, zero-alloc ProcessInto)	SUPPORTED
aeol (port pure-Go WebRTC M145: AEC3 NLMS, NS, AGC2; sem cgo no caminho padrão*)	SUPPORTED
algo-dsp (biquad SIMD AVX2/SSE2/NEON + buffer pool p/ hot paths)	SUPPORTED
PortAudio 8,3 ms e2e; chan []int16 24,7 ms vs RingBuffer 13,1 ms	NOT FOUND — números sem fonte primária; não citar como fato
P99 jitter 127→21 µs com LockOSThread+Ticker	NOT FOUND — estratégia plausível (folklore Go), números não verificados
Buffer pool: 842→11 faults/s; 127→3,2 µs P99	NOT FOUND — não citar
oto/malgo, mempipe WASM, Opus via wazero	UNVERIFIED — budget esgotado, não fetched
Mapa nota → gaps reais do we-calls (aplicabilidade)
1. Jitter buffer adaptativo (nota §5.2) → aplicação direta: servidor tem pass-through puro; cliente tem ring fixo de 2 s sem prefill/adaptação. Ganho real de qualidade de escuta.
2. Zero-alocação / buffer pool (§3.3) → aplicação direta no servidor: há make por pacote em todo hot path (pcm, frame, encoder clean, RTP structs, SRTP out+IV, aes.NewCipher/NewCTR recriados por pacote, HMAC, broadcast slice) — lista completa colhida com path:line pelos slices. A direção (pool/reuso de cipher) é sustentada pelo algo-dsp (SUPPORTED); os números de ganho da nota, não.
3. AEC/NS/AGC (§4.3) → dois degraus: (a) barato, client-side — fixar echoCancellation/noiseSuppression/autoGainControl no getUserMedia (hoje ausente, webrtc.ts:24-26); (b) Go-side — aeol (SUPPORTED) caberia entre Unprotect e Encode/antes do dc.Send, mas é Premissa: custo de CPU por frame de 60 ms ainda não medido aqui.
4. Resampler/simd (§4.1) → não aplicável ao servidor hoje: não há conversão de taxa (tudo 16 kHz; resample.go só normaliza comprimento). Entraria só se fronteiras de taxa divergirem no futuro.
5. Determinismo de runtime (§1) → LockOSThread/GOGC=off estão ausentes do caminho de mídia; a prática é conhecida, mas os números da nota ficam não verificados até benchmark próprio.
6. Backpressure e head-of-line (extra do código, não da nota) → DC ordered+reliable sem maxPacketLifeTime + Broadcast bloqueante sob mutex = pipeline sensível a perda/retransmissão; mitigação seria DC parcialmente não-confiável + drop policy — Correlação: mecanismo claro no código, efeito em latência não medido.
Open Questions
Nenhuma bloqueante. Dúvida aberta rótulo-Premissa: budget do @citer esgotou antes de verificar oto/malgo/mempipe — irrelevante para o caminho atual (o server não faz I/O de dispositivo de áudio; o browser faz).
Proposed Plan
N/A — pedido de entendimento/fundamentação, cumprido. Se quiser evoluir para implementação, ordem natural de alavancas (escopo a decidir): (1) verificar tag HMAC no Unprotect; (2) constraints AEC/NS/AGC no getUserMedia; (3) jitter buffer adaptativo (cliente e/ou servidor); (4) eliminação de allocs por pacote (cipher reuse + pools); (5) aeol server-side. Cada uma merece plano próprio com medição de latência antes/depois — os números da nota não servem como baseline.

## Rodada 2 — Novas ideias de E/S de áudio

Síntese de 4 fatias (@research-analyst, @scientific-literature-researcher, @data-scientist, @citer) contra o estado da Rodada 1. Regra T7: nenhum número derivado é promovido a Fato.

### 2.1 PLC no underrun do downlink
- Ideia: substituir o preenchimento com silêncio do ring (out[i]=0) por concealment — repetir/lerp do último frame com crossfade 5–10 ms no worklet; explorar Decode(nil) do MLow, que já devolve 960 amostras de "PLC de silêncio" e é ignorado pelo playback.
- Evidência: client/public/worklets/playback-processor.js:32-34 (zeros no underrun) + mlow_codec_native_test.go:44-53 (Decode(nil)→960) | NetEq: https://chromium.googlesource.com/external/webrtc/+/master/modules/audio_coding/neteq/g3doc/index.md | Perkins & Hodges ICASSP 1991 (PLC por extrapolação) | veredito @citer: não coberto (PLC não foi alvo de veredito; nenhum veto).
- Aplicabilidade: downlink/browser — playback-processor.js; secundário: estado do decoder MLow no servidor. 3 fatias convergem (T2).
- Confiança: Correlação — mecanismo verificado no código por 2 fatias (fill-zero + endpoint Decode(nil)); ganho perceptual ainda não medido.

### 2.2 DTX/VAD gate no uplink
- Ideia: gate de envio por RMS no capture worklet (cortar tráfego em silêncio de conversa ~60–80%) + comfort noise; validar semântica de "frames ausentes" no relé antes de ativar.
- Evidência: client/src/lib/webrtc.ts:41 (dc.send incondicional a cada 8 ms — corroborado por 2 fatias, T6) | RFC 7587 §3.1.3; RFC 3389/6716 (DTX+CNG padronizados) | veredito @citer: não vetado (o NOT FOUND de gUM refere-se às constraints AEC/NS/AGC, não a DTX).
- Aplicabilidade: uplink mic/browser + contrato do servidor/relé; com DC ordered+reliable o ganho é de banda/CPU, não necessariamente de latência (ressalva da literatura).
- Confiança: Correlação — envio sem gate verificado no código; semântica de frames ausentes no relé WhatsApp não confirmada.

### 2.3 Reframe de 60 ms para 20 ms (960→320 samples)
- Ideia: reduzir o frame do encoder MLow de 960 (60 ms) para 320 (20 ms), alinhando ao default de 20 ms do Opus/RFC 6716 e cortando ~40 ms de buffering por hop (cada transcodificação custa ~21–27 ms — lit. JAIST 2016).
- Evidência: mlow_codec_native_test.go:18 (FrameSize 960) | RFC 6716 (frames 2,5–60 ms, default 20 ms) | veredito @citer: não vetado; repos de DSP/codec SUPPORTED (tphakala/go-audio-resampler v1.9.0; CWBudde/algo-dsp) mas não cobrem o constraint do MLow.
- Aplicabilidade: uplink servidor — internal/voip/media/codec.go:23-26 (constantes mlowSampleRate=16000/mlowFrameSize=960) + buffering/reframe real em internal/voip/call/callmanager_media.go:30-48 (encodeBuf no FeedCapturedPCM) — reconfirmado T6, mlow_codec_native_test.go:18. 2 fatias (T3).
- Confiança: Premissa — se o MLow vendored exige 960, a ideia morre; o ganho −15..−30 ms P50 é estimativa (T7), só aceitável com medição.
- Resultado do spike (execução): FAIL — gate em internal/voip/mlow/encoder.go:627-630 exige exatamente 960 samples; const opusFrameSamps=960 fixa (mlow/decoder.go:12); sem opção de frame size na API. Decode com TOC 20 ms apenas trunca a saída (decoder.go:84-89) — não é frame real. Redefinir o frame exige alterar o código vendored (gate + loop de 3 internal frames de 20 ms + zero-pad analysis.go:102-107). Ideia 2.3 ABORTADA nesta rodada; reavaliar apenas se houver decisão explícita de modificar o codec vendored.

### 2.4 SLO de latência one-way + instrumentação E2E (T0–T9)
- Ideia: adotar SLO explícito (one-way ≤150 ms aceitável, <100 ms imperceptível) e instrumentar o pipeline: timestamps no payload do DC (T0–T8), contadores no worklet (fill/underruns/drops, T9) e profile de mutex/broadcast; baseline antes de qualquer otimização.
- Evidência: ITU-T G.114 (⚠️ conhecimento consolidado, não re-verificado online) | protocolo do @data-scientist (T0–T9; tc-netem 1%+40 ms; CPU 80%; 5×120 s; janelas de 1 s; Mann-Whitney α=0,01 Bonferroni + Cliff's delta bootstrap 10k) | veredito @citer: G.114 existência/norma em vigor SUPPORTED (itu.int/rec/T-REC-G.114, 05/2003 + Amendment 1 "New Appendix II: Guidance on one-way delay for VoIP"); limiares 150/100 ms permanecem Premissa — texto normativo não acessível na página (T1 re-verificado via webfetch direto).
- Aplicabilidade: pipeline inteiro mic→alto-falante; enabler de verificação das demais ideias desta rodada (G4).
- Confiança: Premissa — limiares da G.114 como meta normativa não re-verificada; o protocolo de medição é Correlação (pontos T0–T9 mapeados em código real verificado).

### 2.5 Assincronizar o broadcast (mutex só para snapshot)
- Ideia: tornar assíncrono o loop de envio por peer — o Broadcast em internal/voip/transport/sctprelay.go:345-355 já faz Lock→snapshot→Unlock e envia FORA do lock (:351); o bloqueio remanescente é o loop síncrono sendRaw por peer + bridge.go:84 (dc.Send) — mover envio para fila/goroutine por peer.
- Evidência: cmd/server/session.go:32,213,221 (mutex/broadcast) + callmanager_relay.go:24-33,70-94 (locks do relé) + bridge.go:84 (dc.Send síncrono) — todos reconfirmados T6; correção: envio em sctprelay.go é fora do mutex — todos verificados pelo @data-scientist | veredito @citer: não vetado (claims NOT FOUND de Go real-time referem-se a números de alloc/jitter, não a locks locais).
- Aplicabilidade: servidor downlink — session.go, callmanager_relay.go, bridge.go. Fonte única; magnitude (P99 −20..−50 ms com ≥3 peers) = estimativa (T7).
- Confiança: Correlação (baixa) — mecanismo de bloqueio verificado por path:line; efeito em cauda não medido; medir baseline (2.4) antes de implementar; sem seção em implementacao.md até então.

### 2.6 Batching no capture worklet (8→56 ms)
- Ideia: coalescir 7 quanta de 128 samples antes de postMessage/dc.send (−87% de envios: 125→17/s) em vez de send a cada 8 ms.
- Evidência: client/public/worklets/capture-processor.js:2-8 (quantum 8 ms; contagem de allocs derivada); 128 samples = render quantum default do AudioWorklet (sem literal 128 no código) | veredito @citer: não vetado; números de alloc da nota antiga seguem NOT FOUND e não servem de meta.
- Aplicabilidade: uplink browser — capture-processor.js, webrtc.ts; sub-item do tópico pacing/backpressure já registrado (T5); aumenta latência de envio em até +48 ms — trade-off a medir com 2.4.
- Confiança: Correlação (baixa) — contagem derivada, ganho de CPU não medido; sem seção em implementacao.md até baseline.

### 2.7 Medir fill do ring e dimensionar RING + prefill
- Ideia: instrumentar a profundidade do ring (hoje overwrite-oldest sem contador de drops) e, se P50(fill) > 150 ms, encolher RING 32000→2400–4800 amostras com prefill mínimo (o atraso é o nível de fill — available —, não o tamanho fixo).
- Evidência: client/public/worklets/playback-processor.js:1 (RING 32000 = 2 s), :9 (available), :15-19 (overwrite-oldest sem contador) | veredito @citer: não vetado (jitter buffer/G.114 NOT FOUND por ferramenta → UNVERIFIED, T1).
- Aplicabilidade: extensão quantitativa da ideia 1 (jitter buffer adaptativo) da Rodada 1; playback-processor.js.
- Confiança: Correlação (baixa) — "medir primeiro" é a própria regra; −1,85 s é pior caso, não previsão.

### 2.8 Harness offline de regressão de qualidade (PESQ/POLQA/ViSQOL)
- Ideia: antes de cada mudança de E/S, rodar avaliação objetiva offline (correlatos com MOS validados) para comparar before/after sob o mesmo trace.
- Evidência: GARCÍA et al. 2020, Electronics 9(3):462, DOI 10.3390/electronics9030462 (POLQA/PESQ/ViSQOL ↔ MOS) | CINAR 2020, DOI 10.1145/3410449 (política de descarte seletivo melhora POLQA — apoio a 2.1 e 2.7) | veredito @citer: não coberto; papers verificados por DOI/URL.
- Aplicabilidade: processo, não código — gating de qualquer seção de implementacao.md; Premissa: harness com MLow vendored + relé real é viável offline.
- Confiança: Premissa — base científica verificada; viabilidade no nosso stack não.

### 2.9 Opus 16 kHz via WASM no browser
- Ideia: substituir PCM cru por Opus 16 kHz codificado no browser (~20 kbps vs ~32 kB/s; FEC/PLC/DTX nativos), mantendo o relé como pipe opaco.
- Evidência: https://github.com/openclaw/libopus-wasm (16 kHz, 2025) | wideband MOS 4.27–4.60 vs G.711 ~3.9 (JAIST 2016, verificada) | veredito @citer: não vetado; compatibilidade MLow≈Opus em pass-through não verificada.
- Aplicabilidade: browser + contrato do datachannel; mudança grande de payload — só se 2.1–2.4 forem insuficientes.
- Confiança: Premissa — compat de codec e suporte Safari/Chrome não testados.

### 2.10 WebCodecs (AudioEncoder/AudioDecoder) em Worker
- Ideia: usar WebCodecs com Latency Mode no lugar de AudioWorklet+MLow para codec no browser.
- Evidência: https://www.w3.org/TR/2025/WD-webcodecs-20251124 (§7.3, §12.7; WD nov/2025) | veredito @citer: não vetado; suporte a opus em Chrome/Safari 2025+ não testado.
- Aplicabilidade: browser — depende de 2.9 (só faz sentido com codec no browser).
- Confiança: Premissa — spec verificada; implementação nos browsers não.

### 2.11 Aplicar setSinkId (seleção de saída existe no store, não no elemento)
- Ideia: o store expõe outId/setOut mas não há uso de setSinkId em *.{js,ts} — aplicar a troca no elemento de áudio (alto-falante/earpiece).
- Evidência: client/src/stores/devices.ts:5,7; client/src/hooks/useAudioDevices.ts:12; ausência de setSinkId no grep | veredito @citer: não coberto (achado de código local).
- Aplicabilidade: client/src/components/domain/call/CallCard.tsx:53-69 (áudio/setSinkId candidato — caminho completo confirmado nesta passada); Premissa: grep não inspecionou HTML dinâmico.
- Confiança: Premissa (baixa) — aparente feature pela metade; confirmar no browser antes de tratar como ideia.

### Descartadas/fundidas nesta rodada
- Insertable streams (@research-analyst #8) — DESCARTADA: o browser não usa RTP no caminho (só dc.send; sem addTrack/RTCRtpSender).
- FEC/RED (@research-analyst #9) — DESCARTADA hoje: DC ordered+reliable não perde pacotes; reavaliar apenas se adotar DC não-confiável (previsto na ideia 6 da Rodada 1).
- Telemetria de underrun + time-scaling NetEq (@research-analyst #6) — FUNIDA: extensão da ideia 1 (jitter buffer) já registrada; contadores reutilizados em 2.4/2.7.
- Pacing do dc.send (@research-analyst #7) — FUNIDA: sub-item da backpressure/HOL já registrada (Rodada 1, ideia 6).
- RNNoise/DTLN/DeepFilterNet (lit #8) — REFORÇO, não ideia nova: custo <1% CPU reforça a ideia 3 (AEC/NS/AGC) já registrada.
- Números da nota antiga (PortAudio 8,3 ms; 24,7→13,1 ms; P99 127→21 µs; 842→3,2 faults) — mantidos NOT FOUND; não reutilizados como meta (T7).

### Reconciliação de tensões (T1–T7)
- T1 (atualizado por webfetch direto): RFC 3711 SUPPORTED-condicional — §3.3 passo 5: "If the result is AUTHENTICATION FAILURE ... the packet MUST be discarded"; §4.2: tag divergente → "AUTHENTICATION FAILURE" MUST be returned; ordem replay→autenticação→decrypt. Ressalva: RFC não obriga ATIVAR autenticação (RECOMMENDED; SRTCP mandatory) — como o nosso Protect gera tag, o receive MUST verificar. G.114: existência/norma SUPPORTED; números 150/100 ms = Premissa. Defaults gUM (AEC/NS/AGC): NOT FOUND em MDN MediaTrackConstraints (membro e tipo documentados, default não) — logo, fixar constraints explicitamente permanece a ação correta.
- T2: PLC aprovado em ideias.md (2.1) e em implementacao.md — 3 fatias convergem (2× path:line + Perkins/NetEq).
- T3: reframe 20 ms em ambos (2.3) com Premissa-gate "MLow exige 960?".
- T4: SLO+instrumentação em ambos (2.4); o protocolo do @data-scientist É o plano de implementação.
- T5: #6/#7 fundidas; #8/#9 descartadas (lista acima).
- T6: reconfirmação completa executada (rodada de execução). Correções aplicadas: session.go = cmd/server/session.go; codec.go:23-26 são constantes (reframe real em callmanager_media.go:30-48); callmanager_media.go vai até :56; Broadcast em sctprelay.go envia após Unlock (:351); webrtc.ts:41 condicionado a readyState===open mas sem check de bufferedAmount (sem backpressure); quantum 128 implícito no worklet. srtp.go:97-122 CONFIRM: Unprotect decifra sem comparar a tag (vulnerabilidade confirmada em código).
- T7: nenhum número derivado (68 ms; −15..−30 ms; −20..−50 ms; −87%; −1,85 s) promovido a Fato; todos rotulados estimativa com métrica-alvo.
