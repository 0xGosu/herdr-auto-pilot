# Analysis: bigger embedding model (5k–10k char inputs) + GPU acceleration

**Date:** 2026-07-12 (rev. 2 — re-synced after PR #28 submodule move and PR #34
`pane_salient_chars` + reembed) · **Status:** research only, nothing implemented
**Objective:** replace the default embedding model (all-MiniLM-L6-v2) with one that can
embed the full captured pane content — currently the operator can widen
`embedding.pane_salient_chars` toward the ~5 000-char capture and possibly 10 000 —
and enable GPU (especially Apple Silicon / Metal) so the bigger model stays fast.

5 000 chars of terminal text ≈ **1 300–1 800 tokens**; 10 000 chars ≈ **2 600–3 600
tokens** (terminal output tokenizes densely, ~3 chars/token worst case). So the model
needs a **≥ 2 048-token context for the 5k case and ≥ 4 096 for the 10k case**.

> **Two different "char" knobs — don't conflate them.** The objective's "5000 chars"
> most naturally maps onto `embedding.pane_salient_chars`, which is what actually gets
> embedded. It is a *separate* setting from `llm.pane_excerpt_chars`:
> - **`llm.pane_excerpt_chars`** (default **5 000**, `config.go:237`) — the pane excerpt
>   handed to the **LLM consult**. Never embedded. This is where "5000" already appears
>   in the defaults today.
> - **`embedding.pane_salient_chars`** (default **800**, `config.go:133` →
>   `domain.DefaultPaneSalientChars`) — the trailing window that **is embedded** for
>   idle/unclassified situations. Raising *this* toward 5 000–10 000 is the lever the
>   objective needs, and it is exactly what the small model's 512-token window blocks.
>
> **What changed since rev. 1:** PR #34 replaced the hardcoded 400-char window with this
> configurable `pane_salient_chars` (default raised to 800) and shipped a full
> embedding-drift + re-embed migration story (see §1, §5.3, §5.4). The embedder itself is
> **unchanged** — every blocker in §2 still stands.

---

## 1. Current state (verified in code)

| Fact | Where |
|---|---|
| Default model: `all-minilm-l6-v2-q8_0.gguf` — 384 dims, 512-token window, ~25 MB | `internal/embedder/embedder.go:25`, release.yml `MODEL_URL/SHA256` (lines 12–14) |
| What is embedded today: choices → sorted option list, approvals → `permission:<verb>`, errors → `error:<summary>`, **otherwise the trailing `pane_salient_chars` runes (default 800, rune-aware)** | `internal/domain/signature.go:112` (`salientContent(s, salientChars)`), `signature.go:64` (`DefaultPaneSalientChars = 800`) |
| The fallback window is now **operator-configurable and threaded through every signature call site** (daemon act/sweep/drift-recheck) via `domain.ComputeSignatureN(s, cfg.Embedding.PaneSalientChars)`; TUI-editable. **No upper clamp** — a user can set 5 000/10 000 today | `daemon.go:576,1275,1529`, `sweep.go:168`, `frontend.go:646,746` |
| Embedder creates one long-lived context, `NewContext(llama.WithEmbeddings())` — **still no context-size/batch/ubatch/pooling options passed** (unchanged since rev. 1) | `internal/embedder/embedder.go:101` |
| Per-call stall guard **2 s** once warm, 30 s for warm-up; **5 consecutive failures latch permanent degraded mode** (fixed constant, not configurable) | `embedder.go` |
| Cosine threshold default **0.90**, BM25 min **0.35** — tuned against MiniLM on short strings | `internal/config/config.go` (`SimilarityThreshold: 0.90`, `BM25MinScore: 0.35`) |
| `gpu_layers` config exists and is passed via `llama.WithGPULayers`; negative values are clamped to 0 (so `-1` = "offload all" is not expressible today) | `config.go:123`, `config.go:409`, `embedder.go:96` |
| Model-swap migration is now **mature** (PR #34): drift detection (`frontend.EmbeddingDrift` + `store.CountStaleSignatureEmbeddings`) surfaced in `hap status` and a TUI banner; `hap signatures reembed [--force]` re-computes (daemon-nudge via `control.KindReembed` rebuilds the embedder + clears the degraded latch, or in-process under the daemon lock); `reembed.Reconcile` gained a generation guard (`stale func() bool`) + `PersistFailed` accounting | `internal/reembed/reembed.go:44,55`, `internal/frontend/frontend.go:186–225`, `internal/cli/cli.go`, `internal/control/control.go` |
| Match index is dims-agnostic — rebuilt at init from SQLite with whatever dims the live model reports | `internal/match/match.go:69–139` |
| Official builds compile GPU out: `-tags cpu` + `-DGGML_METAL=OFF` | `scripts/setup-native.sh:70`, release.yml `GO_BUILD_TAGS` |
| llama-go submodule now lives at `submodule/github.com/seed-hypermedia/llama-go` (PR #28 merged); vendored llama.cpp is **b7855** (2026) — new enough for ModernBERT (merged Dec 2025) and the per-context Metal shader-recompile fix (Mar 2025) | `go.mod:69`, `.gitmodules` |

**Implication for the objective:** PR #34 delivered the *knob* — `pane_salient_chars`
now lets an operator ask for a 5 000-char embedding window — but the model and embedder
did **not** change, so widening it past ~512 tokens (~1 500–2 000 chars) hits the §2
blockers immediately. In other words the codebase now *invites* the exact failure this
work must unblock. Two gaps remain: (a) the embedder/model can't accept the wider window
(§2), and (b) **structured** situations (choice/approval/error) still collapse to short
strings — embedding their full capture is still the deferred pipeline change (§5.3).

---

## 2. Hard blockers in the current llama-go stack

These were verified by reading `submodule/github.com/seed-hypermedia/llama-go`
(wrapper.cpp + options):

1. **`n_ubatch` is never set** (wrapper.cpp:121 sets only `n_batch`; no Go option
   exists). llama.cpp asserts `non-causal attention requires n_ubatch >= n_tokens`
   ([#18757](https://github.com/ggml-org/llama.cpp/issues/18757)) — every
   bidirectional-encoder model (BERT/XLM-R family: bge-m3, nomic, arctic, granite,
   EmbeddingGemma) must fit the whole input in one micro-batch. With the default
   `n_ubatch = 512`, **any input over 512 tokens fails or is mis-embedded** no matter
   how large the model's context is. Fix: plumb `n_ubatch` (set `= n_batch = n_ctx`)
   through wrapper.cpp + a `WithUBatch`/extended `WithBatch` option.
2. **The wrapper chunk-decodes embeddings in 512-token pieces**
   (wrapper.cpp:1007–1050, `llama_wrapper_embeddings`). For bidirectional models,
   pooled sequence embeddings are computed per `llama_decode` call — chunking a long
   input means the returned vector likely reflects **only the last chunk** (silent
   wrong-answer, not an error). Needs verification with a real >512-token input, but
   the code path supports no other conclusion. The default 800-char salient window
   (~200–270 tokens) still stays under one chunk — but PR #34 lets an operator set
   `pane_salient_chars` past 512 tokens with **no clamp**, so this is now a
   user-reachable silent-corruption path, not a hypothetical.
3. **No pooling-type option.** The wrapper reads `llama_get_embeddings_seq(ctx, 0)`
   (wrapper.cpp:1052) and relies on the GGUF declaring its pooling. Encoder GGUFs do;
   **decoder-based embedders (Qwen3-Embedding, jina-code-embeddings) resolve to
   pooling `none`** and official usage passes `--pooling last` explicitly — llama-go
   cannot express that today, so those models would return errors (or garbage).
4. **No input-length guard in hap.** Tokens beyond `n_ctx` fail inside
   `llama_decode`; three long inputs in a row would trip the degraded latch and
   permanently disable semantic matching. hap must truncate (by tokens ideally,
   or conservatively by chars ≈ 3×n_ctx) before calling the embedder.
5. **The 2 s stall guard is sized for MiniLM.** A 300–600 M model embedding 3 000
   tokens on CPU costs ~1–4 s on a modern x86 box and **~5–8 s on a base M-chip CPU**
   (extrapolated, see §4) — the guard would latch degraded mode on the very inputs
   this project targets. `embedTimeout` must become config- or model-aware.

None of these are model-choice-dependent except #3 (decoder models only).

---

## 3. Candidate models (web research, July 2026)

Full tokens-and-sources table from the research pass:

| Model | Params | Dims (MRL) | Max ctx | Arch | Prefixes | Official GGUF | Q8_0 | License |
|---|---|---|---|---|---|---|---|---|
| Qwen3-Embedding-0.6B | 0.6 B | 1024 (32–1024) | 32 768 | causal | optional | **yes** | 639 MB | Apache 2.0 |
| EmbeddingGemma-300m | 300 M | 768 (128–512) | **2 048** | encoder | required | yes (ggml-org) | 329 MB | Gemma terms |
| nomic-embed-text-v1.5 | 137 M | 768 (256/512) | 2 048 in llama.cpp (8 k needs untested RoPE flags) | encoder | required | **yes** | 146 MB | Apache 2.0 |
| nomic-embed-text-v2-moe | 475 M | 768 | **512** | encoder | required | yes | 478 MB | Apache 2.0 |
| **bge-m3** | 568 M | 1024 | **8 192** | encoder | **none** | third-party | 635 MB | MIT |
| **arctic-embed-l-v2.0** | 568 M | 1024 (**MRL 256**) | **8 192** | encoder | query-side only | third-party | ~600 MB | Apache 2.0 |
| jina-embeddings-v2-base-en/-code | 137/161 M | 768 | 8 192 (ALiBi) | encoder | none | third-party, **llama.cpp regressions** | 173 MB | Apache 2.0 |
| **granite-embedding-english-r2** | 149 M | 768 | **8 192** | encoder (ModernBERT) | none | pending (self-convert) | ~160 MB | Apache 2.0 |
| **jina-code-embeddings-0.5b** | 494 M | 896 (64–896) | 32 768 | causal | required (task-specific) | **yes** | ~530 MB | Apache 2.0 |

Key sources: [Qwen3-Embedding-0.6B-GGUF](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF) ·
[nomic-v1.5-GGUF](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5-GGUF) ·
[BAAI/bge-m3](https://huggingface.co/BAAI/bge-m3) ·
[arctic-embed-2.0 blog](https://www.snowflake.com/en/engineering-blog/snowflake-arctic-embed-2-multilingual/) ·
[granite-r2](https://huggingface.co/ibm-granite/granite-embedding-english-r2) ·
[jina-code-embeddings](https://jina.ai/news/jina-code-embeddings-sota-code-retrieval-at-0-5b-and-1-5b/) ·
[llama.cpp #19933 (speed)](https://github.com/ggml-org/llama.cpp/issues/19933)

### Ruled out

- **nomic-v2-moe** — 512-token context, same ceiling we're escaping.
- **EmbeddingGemma-300m** — 2 048 ceiling fails the 10k-char goal (and can clip bad-tokenizing 5k inputs); restrictive license; rigid prompt templates.
- **jina-v2-base-\*** — jina-bert-v2 has open correctness/crash issues in llama.cpp
  ([#16392](https://github.com/ggml-org/llama.cpp/issues/16392),
  [#18452](https://github.com/ggml-org/llama.cpp/issues/18452)).
- **nomic-v1.5** — only if 2 048 ctx is accepted; 8 k requires RoPE-scaling flags the
  model card itself calls untested.
- **Qwen3-Embedding-0.6B** — viable but unattractive: ~5× slower per token than
  bge-m3 in llama.cpp embedding serving
  ([#19933](https://github.com/ggml-org/llama.cpp/issues/19933): ~6.9 k vs ~31 k tok/s
  same rig), needs the pooling-last option (§2.3), needs explicit `<|endoftext|>`
  handling ([#14234](https://github.com/ggml-org/llama.cpp/issues/14234)), 639 MB.

### Shortlist

A note that matters for ranking: hap's matching is **symmetric** (stored salient text
vs. new salient text — same kind of text on both sides), not query→document retrieval.
Models with asymmetric prefixes (nomic, arctic query-side) are usable by picking one
prefix for both sides, but no-prefix models (bge-m3, granite-r2) fit naturally.

1. **bge-m3** — the battle-tested choice. 8 192 native ctx, no prefixes, MIT, and the
   *fastest* per-token encoder in the llama.cpp comparison above. Costs: 635 MB Q8
   (438 MB Q4_K_M), 1024-dim vectors (no MRL), community GGUF (we'd pin our own
   sha256 or self-convert — XLM-R arch is mature in llama.cpp).
2. **granite-embedding-english-r2** — best quality-per-MB *if* we self-convert:
   149 M/~160 MB, 768 d, 8 192 ctx, no prefixes, strong on code/long-doc benchmarks,
   Apache 2.0. ModernBERT needs llama.cpp ≥ Dec 2025 — our b7855 qualifies. Risk:
   arch is young in llama.cpp; no official GGUF yet (IBM promised).
   English-only — a regression vs. bge-m3 for non-English operators.
3. **arctic-embed-l-v2.0** — bge-m3-class body with **MRL-256** (4× smaller vectors
   in SQLite/FAISS); Apache 2.0. Third-party GGUF only; asymmetric `query:` prefix
   (tolerable in symmetric use by omitting it consistently).
4. **jina-code-embeddings-0.5b** — the quality leader for code/terminal-flavored text
   (78.4 % avg over 25 code-retrieval benchmarks, ~5 pts above Qwen3-0.6B). Official
   GGUF, causal arch (escapes the ubatch constraint entirely). Costs: needs the
   pooling-last option (§2.3), task-instruction prefixes, and decoder-speed on CPU.

**Recommendation:** default to **bge-m3 Q8_0** (or Q4_K_M for a 438 MB download) with
`n_ctx = n_batch = n_ubatch = 4096`; keep `granite-r2` as the lean alternative once an
official GGUF lands; benchmark `jina-code-embeddings-0.5b` as a possible
quality upgrade after the pooling option exists. All three keep working with the
existing `model_path` override for users who choose differently.

---

## 4. GPU acceleration (web research, July 2026)

Embedding is **prompt-processing (pp) bound** — exactly the metric where GPUs shine;
token-generation numbers are irrelevant here.

### Apple Silicon / Metal — clear win

- Measured, closest analog: **Gemma-3 1B Q4_0 on base M1** — pp 454 t/s CPU vs
  1 031 t/s Metal = **2.27×** ([#12985](https://github.com/ggml-org/llama.cpp/discussions/12985)).
  On Max-class chips the 7B measurements show **~5.3×**
  ([#4167](https://github.com/ggml-org/llama.cpp/discussions/4167)); pp scales with
  GPU core count.
- ggerganov's embedding tutorial: a 33 M encoder on Metal goes from 1.8 k t/s at
  8-token inputs to **~31 k t/s at 256 tokens**
  ([#7712](https://github.com/ggml-org/llama.cpp/discussions/7712)) — GPU
  underutilization only matters below ~64-token inputs; our 1 300–3 600-token inputs
  are deep in GPU-favorable territory.
- Practical expectation (extrapolated, flagged as such by the research): a
  3 600-token embed with a ~600 M Q8 model ≈ **5–8 s on base-M CPU → ~2–3 s Metal on
  base chips, well under 1 s on Pro/Max**. On x86 CPU ≈ 1–4 s.
- **Memory:** Metal maps weights zero-copy from unified memory (no doubling); the
  compute buffer grows with ubatch — expect ~0.9–1.5 GB total resident for a 600 M
  model with a 4 096 ubatch (estimate). Fine on any 8 GB+ Mac.
- **Static-binary shipping works:** `GGML_METAL_EMBED_LIBRARY` (default ON with
  `GGML_METAL` since June 2024, [PR #8006](https://github.com/ggml-org/llama.cpp/pull/8006))
  embeds the shader source into the archive; our single-context daemon avoids the old
  per-context recompile issue (fixed Mar 2025 anyway, and b7855 is newer).
- `n_gpu_layers`: all-or-nothing offload is the right call for a ≤ 640 MB model
  (`-ngl 99`-equivalent). Partial offload has no upside here.

### Linux / Vulkan — not worth it by default

iGPU-class Vulkan pp is slower than a good many-core CPU
([#10879](https://github.com/ggml-org/llama.cpp/discussions/10879)); dropping the
`cpu` tag on Linux would also hard-link `libvulkan` (runtime driver dependency) via
llama-go's `zgpu_linux.go`. Keep Linux CPU-only; revisit if users ask for
discrete-GPU support.

### What enabling Metal takes (from the earlier build-layer analysis)

Already-verified facts: the `cpu` Go build tag is the only thing suppressing
llama-go's Metal linkage (`zgpu_darwin.go`: `-lggml-metal -lggml-blas` + Metal
frameworks); `WithGPULayers` plumbing exists end-to-end.

1. `scripts/setup-native.sh` (darwin): build llama-go with `BUILD_TYPE=metal`
   (Makefile adds `-DGGML_METAL=ON`, copies `libggml-metal.a` + `libggml-blas.a`)
   instead of `-DGGML_METAL=OFF -DGGML_ACCELERATE=OFF`.
2. Per-target build tags: darwin `-tags vectors`, linux `-tags "vectors cpu"` — move
   `GO_BUILD_TAGS` from workflow env into the release matrix. The macos-14 CI job
   then exercises the Metal link (GH Apple Silicon runners expose Metal).
3. Config: either allow `gpu_layers = -1` = offload-all (today clamped to 0 at
   `config.go:398`), or document "any value ≥ layer count offloads all".
4. Default: with a big model, darwin should probably default to **full offload**
   (with `gpu_layers = 0` as the escape hatch); Linux default stays 0. This is a
   product decision — flagged in §6.

---

## 5. Full change inventory (what shipping this actually touches)

### 5.1 llama-go fork (`submodule/github.com/seed-hypermedia/llama-go`)
- Plumb `n_ubatch` through `llama_wrapper_context_params` + wrapper.cpp:121 and add a
  Go context option; embedder sets `n_ctx = n_batch = n_ubatch` (4096 recommended).
- Stop chunk-decoding in `llama_wrapper_embeddings` when the model is non-causal
  (whole input in one batch), or error clearly when input > ubatch.
- Add `WithPooling` (needed only if a causal embedder is ever chosen — defer if
  bge-m3/granite is picked).

### 5.2 Embedder (`internal/embedder`)
- Pass `WithContext(n)`, `WithBatch(n)` (+ new ubatch option) sized from config.
- Truncate input before embedding (token-aware if llama-go exposes tokenize-count;
  else conservative char cap ≈ 3 × n_ctx) — never let long input become an error that
  feeds the degraded latch.
- Make `embedTimeout` config-derived (e.g. default 15 s for a big model on CPU, lower
  when `gpu_layers > 0`) — the current 2 s guard is the biggest correctness landmine.
- `warmTimeout` 30 s is probably still fine (mmap load), verify on first-run download.

### 5.3 Signature pipeline (`internal/domain`) — partially delivered by PR #34
- **Idle/unclassified is now done (the knob exists):** `salientContent(s, salientChars)`
  takes the trailing `pane_salient_chars` runes (default 800), and it flows into both the
  signature *and* the embedding. Raising the config is the operator-facing lever the
  objective wants — it's just gated on §2 and on the model window. This work should mainly
  **add safety around that knob**: an input-length guard/truncation in the embedder (§5.2)
  and, ideally, a soft warning in `hap status`/TUI when `pane_salient_chars` exceeds what
  the loaded model can embed.
- **Structured situations are still short** — choices → option list, approvals →
  `permission:<verb>`, errors → `error:<summary>` (`signature.go:113–129`). If embedding
  the *full capture* for these types is wanted, that remains the deferred change: keep the
  cheap deterministic salient string for exact-hash/BM25 and add the **masked full
  excerpt** as the embedded text (replace `Salient`, or add a second `context_text` field
  so BM25 keeps operating on the short form). This is a product decision (§6.3) — the
  cheap strings may already be the right key for menus.
- **Privacy (still open, now higher-stakes):** masking applies to the salient string, and
  `signature_embeddings.salient` persists it in plaintext for re-embedding. With
  `pane_salient_chars` now operator-raisable, a wide window already persists more masked
  pane content — verify `MaskVolatile` holds up on 5 000-char inputs (more surface for
  unmasked secrets) before encouraging users to widen it.
- Over-mask guard thresholds (`overMaskVerdict`, `overMaskMaxRatio = 0.6`) were tuned for
  short strings — re-check ratios for 5 000-char inputs.

### 5.4 Matching & thresholds
- `similarity_threshold = 0.90` was tuned for MiniLM on short strings. Longer texts +
  a different model shift the cosine distribution — re-tune with a small corpus
  (the `e2e_harness` + `test/samples/` panes are a natural seed). Same for
  `bm25_min_score` if the indexed text changes.
- Vector size grows 384 → 768/1024 floats (~1.5 → 3–4 KB/row + FAISS index) —
  negligible at hap's row counts; arctic's MRL-256 would even shrink it.
- Migration is **well-handled now (PR #34)**: model drift is detected
  (`EmbeddingDrift` / `CountStaleSignatureEmbeddings`), surfaced in `hap status` + a TUI
  banner, and re-embedding runs either via daemon nudge (`control.KindReembed` rebuilds
  the embedder and clears the degraded latch) or in-process (`hap signatures reembed
  [--force]`), with a generation guard so a superseded pass can't clobber fresher vectors.
  A model swap thus self-heals: on first start under the new model, drift shows and
  reembed re-computes every row. **Caveat unchanged:** rows are re-embedded from their
  *stored* salient text, which for pre-existing idle rows is the *old, narrower* window —
  grandfathered rows match on old-width vectors until the situation recurs and re-learns.
  This makes phase 2 (model swap) materially lower-risk than at rev. 1.

### 5.5 Distribution & docs
- release.yml `MODEL_URL/FILE/SHA256` → new model (pin sha256; if third-party GGUF,
  consider re-hosting under the project's HF account or converting in CI for
  provenance). `DefaultModelFile` const, install.sh "25MB" comment, sample/config.toml.
- Download grows 25 MB → 146–640 MB: install.sh already treats the model as optional
  (BM25 fallback) — keep that, but add a size warning and maybe a
  `HAP_SKIP_MODEL=1` escape hatch.
- Disk/RAM: daemon RSS grows by roughly the model size (+compute buffers) whenever
  embedding is enabled. Document.
- Integration tests: `TestRealEmbeddingSemanticMatch` (cosine ≥ 0.90, MiniLM file
  name) and the real-model embedder test key off `models/all-minilm-l6-v2-q8_0.gguf`
  / `HAP_TEST_EMBED_MODEL` — both need the new default + threshold.

### 5.6 GPU build (§4) — independent of the model swap, but the model swap is what
makes it worth doing. Ship Metal-capable darwin binaries either way.

---

## 6. Open decisions (need product input)

1. **Model pick:** bge-m3 (recommended default) vs granite-r2 (lean, English-only,
   self-convert) vs jina-code-0.5b (code-max, more plumbing). Multilingual needed?
2. **Quant:** Q8_0 (max quality, 635 MB) vs Q4_K_M (438 MB, ~1–2 % retrieval loss).
3. **What text gets embedded per situation type** (§5.3) — idle/unclassified is already
   the trailing `pane_salient_chars` window (PR #34). Still to decide: leave structured
   choice/approval/error on their short deterministic strings (likely correct for menus),
   or also embed their full masked excerpt where the short form loses signal?
4. **darwin GPU default:** full offload by default vs opt-in. (Recommended: default
   full offload once the big model ships; `gpu_layers = 0` opt-out.)
5. **10k-char goal timing:** n_ctx 4096 covers it; going to 8192 doubles the ubatch
   compute buffer — decide whether to ship 4096 first.
6. **GGUF provenance:** re-host/convert bge-m3 ourselves vs pinning a community file
   by sha256.

## 7. Suggested phasing

1. **Phase 0 (unblocks everything):** llama-go `n_ubatch` + non-chunked encoder path;
   embedder context/batch options + input truncation + configurable timeout. All
   testable on Linux CPU with the current MiniLM (no behavior change). **Cheap add now
   that `pane_salient_chars` is unclamped:** a guard that truncates (or warns) when the
   configured window exceeds the loaded model's token budget — prevents today's
   user-reachable silent-corruption/degraded-latch path (§2.2, §2.4).
2. **Phase 1 (Metal):** setup-native.sh + per-target tags + release matrix; ship
   Metal-capable darwin binaries with `gpu_layers` opt-in. Verify on a real Mac.
3. **Phase 2 (model swap):** new default model + thresholds + release/install/docs +
   test retuning. **Migration now self-heals** via PR #34's drift-detect + reembed —
   first daemon start under the new model shows drift and re-embeds every row.
4. **Phase 3 (pipeline, optional):** the `pane_salient_chars` window already covers
   idle/unclassified once phases 0–2 land; this phase is only needed if we also want
   structured situations to embed their full masked excerpt (§5.3). Flip the darwin GPU
   default and re-tune thresholds on the longer texts here.

Phases 1 and 2 are independently shippable. Once phases 0–2 land, an operator raising
`pane_salient_chars` toward 5 000 **is** the objective realized for idle/unclassified
content; phase 3 extends it to structured situations only if wanted.
