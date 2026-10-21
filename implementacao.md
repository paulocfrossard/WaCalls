# Plano de implementação — E/S de áudio (we-calls)

Ordem de execução: (1) HMAC Unprotect [segurança] → (2) instrumentação T0–T9 + SLO [enabler de verificação] → (3) PLC no underrun [quick win] → (4) reframe 60→20 ms [gated por MLow] → (5) DTX/VAD [gated por semântica do relé]. Ideias 2.5–2.11 de ideias.md NÃO têm seção aqui até baseline da §2 (protocolo do coordinator §1/§5).

## 1. Correção HMAC no SRTP Unprotect (prioridade 1 — item conhecido, Rodada 1)

### Pré-requisitos
- RFC 3711 como referência normativa — SUPPORTED-condicional (webfetch direto): §3.3 passo 5 exige descartar o pacote em AUTHENTICATION FAILURE; §4.2 exige retornar AUTHENTICATION FAILURE quando as tags divergem; ordem replay→verificação→decrypt. Ressalva: ativação de autenticação é RECOMMENDED na RFC (SRTCP é mandatory) — como o nosso Protect emite tag, o receive MUST verificar.
- Testes unitários de SRTP existentes (estender).

### Passos
1. Extrair a tag recebida do pacote e recalcular o HMAC sobre o payload com a sessão correspondente → arquivo-alvo: srtp.go:97-122 (Unprotect).
2. Comparar em tempo constante (crypto/subtle.ConstantTimeCompare) e rejeitar com erro quando divergir (hoje só a forma do pacote é checada) → arquivo-alvo: srtp.go:97-122.
3. Garantir que computeAuthTag é chamado também no caminho de recepção (hoje só a partir de Protect) → arquivo-alvo: srtp.go:161-169.
4. Propagar a rejeição: pacote com tag inválida não avança para decode/relay → arquivo-alvo: internal/voip/call/callmanager_media.go:146 (pkt, err := srtp.Unprotect(data)) — reconfirmado T6.
5. Estender testes: tag alterada, tag truncada, tag de outra sessão, payload forjado que passe nos filtros de forma → arquivo-alvo: srtp_test.go (criar/estender).

### Riscos
- Rejeitar tráfego legítimo se IV/ROC/state divergirem entre relé e pontas → validar com chamada real antes de fechar gate.
- Overhead de HMAC por pacote no downlink hot path — medir (abaixo), não assumir.
- Todas as path:line da §1 reconfirmadas em T6 (srtp.go:97-122 e :161-169 CONFIRM; callmanager_media.go:146 CONFIRM).

### Critério de verificação
- Funcional: 1000 pacotes com tag alterada/truncada → 0 aceitos; 0 rejeições falsas em 5×120 s de chamada limpa em LAN.
- Latência (protocolo do @data-scientist): Δ one-way P50/P99 < 5 ms e < 10% vs baseline; Mann-Whitney α=0,01 (Bonferroni) sem significância de piora; Cliff's delta < 0.147 (negligible).

## 2. Instrumentação E2E + SLO one-way (ideia 2.4)

### Pré-requisitos
- Decisão de contrato do payload do DC para timestamps (versão/flag obrigatória).
- Baseline capturada ANTES de qualquer outra mudança desta lista.

### Passos
1. Inserir timestamp T0 no worklet de capture → arquivo-alvo: client/public/worklets/capture-processor.js:2-8.
2. Propagar T0 pelo caminho browser→servidor (campo no header do PCM) → arquivo-alvo: client/src/lib/webrtc.ts:40-42 (dc.send no onmessage) — reconfirmado T6, internal/voip/media/pcm.go:16-23 (PCMInt16LEToFloat32; alloc em :17) — reconfirmado T6.
3. Marcar T1–T4 no uplink do servidor (OnMessage, feed, encode, protect) → arquivo-alvo: internal/voip/call/callmanager_media.go:22-56 (FeedCapturedPCM sob m.mu; reframe/encodeBuf em :30-48) — reconfirmado T6, internal/voip/media/codec.go:23-26 (constantes mlowFrameSize=960; reframe de fato em callmanager_media.go:30-48) — reconfirmado T6, internal/voip/media/rtp.go:174-187 (NewRtpSession PT=120/16k/960 em :175; timestamp += durationSamples em :187) — reconfirmado T6.
4. Marcar T5–T8 no downlink (onRelayData, decode, dc.Send) → arquivo-alvo: internal/voip/call/callmanager_media.go:146 (Unprotect) — reconfirmado T6, bridge.go:84 (verificado), cmd/server/session.go:90-96 (OnPeerAudio → WritePCM; erro descartado em :95) — reconfirmado T6.
5. Contadores no playback: nível de fill, underruns, drops (overwrite-oldest hoje sem contador) → arquivo-alvo: client/public/worklets/playback-processor.js:9, :15-19, :28-34.
6. Profile de mutex/broadcast + duração do Broadcast → arquivo-alvo: cmd/server/session.go:32,213,221, internal/voip/call/callmanager_relay.go:24-33,70-94, internal/voip/transport/sctprelay.go:345-355 (Lock→snapshot→Unlock; envio síncrono FORA do lock em :351 — reconfirmado T6).
7. Exportar série T0–T9 e rodar o protocolo: LAN / tc-netem 1%+40 ms / CPU 80%; 5×120 s por condição; agregação em janelas de 1 s; Mann-Whitney α=0,01 (Bonferroni) + Cliff's delta bootstrap 10k → ferramenta: script de análise a criar (ex.: tools/latency/).
8. Fixar o SLO no repositório (one-way ≤150 ms aceitável / <100 ms meta — G.114 = Premissa, UNVERIFIED) e publicar a baseline → arquivo-alvo: ideias.md (resultado) + doc de SLO.

### Riscos
- Timestamps no payload mudam o contrato do DC — flag de versão; validar se o relé inspeciona tamanho de mensagem.
- Custo de instrumentação na hot path — contadores agregados, nunca strings por pacote.
- Sincronismo relógio browser↔servidor: eco/id-ping (±5 ms), não NTP; viés de sync vira viés no SLO.

### Critério de verificação
- Cobertura: T0–T9 presentes em ≥95% dos pacotes em 5×120 s; perda de timestamps < 5%.
- Baseline publicada com P50/P95/P99 por segmento e nível do ring; gates de pré-registro Δ ≥ 5 ms ou ≥ 10% para qualquer otimização futura.
- Estimativa da Rodada 1 (one-way nominal ≈ 68 ms + nível do ring) tratada como Premissa — aceitar/rejeitar apenas com Mann-Whitney α=0,01.

## 3. PLC no underrun do playback (ideia 2.1 / H3)

### Pré-requisitos
- Contadores de underrun da §2 (antes/depois).
- Decisão de estratégia: (a) repeat-last-frame + crossfade 5–10 ms no worklet [client-only, preferida]; (b) reutilizar Decode(nil) do MLow — já devolve 960 amostras (mlow_codec_native_test.go:44-53) [exige contrato servidor↔client para frame "concealed"].

### Passos
1. Manter buffer dos últimos N samples decodificados (reusar o ring existente) → arquivo-alvo: client/public/worklets/playback-processor.js:15-19.
2. No branch de underrun (hoje out[i]=0), emitir concealment com crossfade no lugar do silêncio; degradar para silêncio após 1–2 frames (60–120 ms) → arquivo-alvo: client/public/worklets/playback-processor.js:28-34.
3. Contar underruns "concealed" vs "silenciados" para A/B → arquivo-alvo: idem (contadores da §2).
4. (Opcional, gated pela decisão (b)): plugar saída de Decode(nil) como frame especial → arquivo-alvo: internal/voip/media/mlow_codec_native.go:25 (func Decode(frame []byte) ([]float32, error)); silêncio/PLC = internal/voip/mlow/decoder.go:56-59 com opusFrameSamps=960 (decoder.go:12); teste mlow_codec_native_test.go:44-55 — reconfirmado T6.

### Riscos
- Repetição prolongada cria "robô"/percussão — limite rígido de 1–2 frames.
- Crossfade mal feito introduz clipping/zipper noise — normalizar amplitude do frame repetido.
- Opção (b) acopla o client ao formato MLow — só após medida.

### Critério de verificação
- Não-regressão de underruns: contagem de underruns Δ ≤ 0 (Mann-Whitney α=0,01) no mesmo trace com underruns induzidos por tc-netem.
- Latência: Δ one-way P99 < 5 ms e < 10% (protocolo da §2).
- Qualidade (interim): 5×120 s LAN + 5×120 s netem sem zipper/clipping audível (escuta humana + contagem de amostras saturadas); quando o harness da ideia 2.8 existir, A/B com PESQ/POLQA sob o mesmo trace.

## 4. Reframe 960→320 (60→20 ms) (ideia 2.3 / H1) — GATED

> **STATUS: ABORTADA (spike FAIL).** O MLow vendored exige 960 samples (encoder.go:627-630, const opusFrameSamps=960 em decoder.go:12; API sem opção de frame size; decode TOC 20 ms = truncamento de saída, decoder.go:84-89). Os passos abaixo ficam preservados como referência futura, condicionados a decisão explícita de alterar o codec vendored.

### Pré-requisitos
- PROVA de que o MLow vendored aceita FrameSize 320 (hoje mlow_codec_native_test.go:18 = 960; se exigir 960, abortar e registrar o resultado em ideias.md).
- Baseline da §2 (sem ela o ganho −15..−30 ms P50 é apenas estimativa — T7).

### Passos
1. Spike: encoder/decoder MLow com 320 samples em teste unitário → arquivo-alvo: mlow_codec_native_test.go:18.
2. Se OK: trocar o reframe alvo do uplink 960→320 → arquivo-alvo: internal/voip/media/codec.go:23-26 + callmanager_media.go:30-48 (onde o 960 é aplicado) — reconfirmado T6.
3. Ajustar timestamp RTP (+320 ts/pcto em vez de +960) → arquivo-alvo: internal/voip/media/rtp.go:174-187 — reconfirmado T6.
4. Ajustar decode/NormalizeFrame para o novo comprimento → arquivo-alvo: internal/voip/media/resample.go:3-10 (NormalizeFrame pad/truncate), internal/voip/media/pcm.go — reconfirmado T6.
5. Re-rodar baseline comparativa (antes/depois) nas 3 condições do protocolo.

### Riscos
- Premissa central: MLow pode exigir 960 — spike barato, decidir cedo.
- 3× mais pacotes RTP/DC (overhead de header/HMAC por pacote) — medir CPU com a §1 já aplicada.
- Interop com instâncias antigas (payload incompatível) — versionar PT ou payload.

### Critério de verificação
- Ganho: Δ one-way P50 ≤ −15 ms com Mann-Whitney α=0,01 (Bonferroni) e Cliff's delta ≥ 0.33 (medium) em LAN; sob netem, P99 não piora > 10%.
- Custo: Δ CPU por pacote < 10% no uplink (profile mutex+allocs da §2); underruns do playback Δ ≤ 0.
- Se ganho < 5 ms ou não significativo → reverter e marcar "não valeu" em ideias.md (pré-registro do @data-scientist).

## 5. DTX/VAD gate no uplink (ideia 2.2)

### Pré-requisitos
- Confirmar semântica do relé WhatsApp para intervalos sem mensagens "pcm" (CORRELAÇÃO hoje — se o relé exige tick/heartbeat, abortar ou limitar o gap).
- Contagem baseline de pacotes/s (§2).

### Passos
1. VAD por RMS + hangover no worklet (limiar inicial −40 dBFS, hangover 200 ms; calibrar com gravações reais) → arquivo-alvo: client/public/worklets/capture-processor.js (gate antes do postMessage).
2. Enviar frame só acima do limiar; comfort noise opcional a cada 100–200 ms (ruído derivado do último frame, sem codec novo) → arquivo-alvo: idem + client/src/lib/webrtc.ts:40-42 — reconfirmado T6.
3. Lado servidor: tratar lacunas de tempo como pausa, sem assumir continuidade de timestamp → arquivo-alvo: resample.go:3-10, internal/voip/call/callmanager_media.go:22-56 — reconfirmado T6.
4. Medir: pacotes/s antes/depois (conversa com pausas vs monólogo) e underruns do playback (não devem subir).

### Riscos
- Corte agressivo apaga finais de palavra (tail clipping) — hangover obrigatório; teste com 10 frases reais.
- Timeouts do relé/servidor por ausência de mensagens — Premissa central, validar primeiro.
- Ganho de banda não necessariamente vira ganho de latência com DC ordered+reliable — expectativa correta: CPU/bytes.

### Critério de verificação
- Tráfego: ≥ 50% de pacotes suprimidos em conversa com pausas; ≥ 30% em monólogo (contagens T0–T9).
- Qualidade: underruns do playback Δ ≤ 0 (Mann-Whitney α=0,01); zero tail-clipping em 10 frases de teste (escuta + inspeção de waveform).
- Latência: Δ one-way não piora > 5 ms nem > 10% (protocolo da §2).
