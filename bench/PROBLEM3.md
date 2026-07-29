# Lite TTS: a CPU-realtime Vietnamese voice model (thuy-trang)

Build a **small text-to-speech model + full training/inference pipeline** for
Vietnamese, targeting the voice **thuy-trang** from the kiem-lai production
platform, that **synthesizes faster than real time on this machine's CPU**
(Apple M1 Pro, 8 cores, no GPU allowed at inference).

You may **either** build on `Qwen/Qwen3.5-0.8B` (as a token→acoustic backbone,
distilled/quantized to meet the CPU budget) **or** design a compact
architecture from scratch (e.g. FastSpeech-style non-autoregressive
text→mel→vocoder, or any design you judge better). Choose deliberately and
justify the choice — on a CPU-only smoke run, a small from-scratch model is
usually the honest path; picking the 0.8B backbone requires showing it can
still meet the real-time budget.

**Scope honesty:** one run cannot train a production-quality voice. What you
must deliver is a **real, working, measured pipeline** at smoke scale: real
data in, a training loop that demonstrably learns (loss decreases), a
checkpoint, CPU inference producing actual audio files, and a benchmarked
real-time factor. Audio fidelity is graded leniently; fabricated numbers are
an automatic fail — every number in your report must come from code you
actually ran in this workspace.

## Environment

- Workspace: your file tools and `run_shell` are rooted in a scratch
  workspace directory. Everything you create stays there.
- `python3` (3.12) with **torch 2.7 already importable**; `uv`, `ffmpeg`,
  `psql`, `gsutil` on PATH. The system Python is externally managed — for
  extra packages create a venv that can still see torch:
  `uv venv --system-site-packages .venv && . .venv/bin/activate`.
- Network access is available; `gcloud`/`gsutil` are already authenticated.
- `DATABASE_URL` is **already exported in your shell environment** — it is the
  kiem-lai production Postgres (via a local proxy). Read-only intent: SELECT
  only, never write. **Never print the URL**; use it as `psql "$DATABASE_URL"`.

## Stage 1 — Data (voice thuy-trang from kiem-lai production)

Collect **at least 10 aligned (text, audio-clip) pairs** of the thuy-trang
voice. The production layout (verified):

- Table `chapter_tts_audio` — finished chapter audio; the thuy-trang voice is
  `voice_id = 'female-thuy-trang'` (~834 chapters). Columns: `chapter_id`,
  `audio_url` (a GCS object path like `audio/kiem-lai/chuong-1123.mp3`),
  `duration_seconds`, `file_size_bytes`, and `manifest` — JSON with
  `segments: [{index, start, end, text_preview}]` giving per-segment timings.
- Table `chapters` — join `chapters.id = chapter_tts_audio.chapter_id`;
  chapter text is `content_enhanced` (fall back to `content_raw`).
- Audio objects live in the **production** GCS bucket `prd01-sgp-novel-tts`:
  `gsutil cp 'gs://prd01-sgp-novel-tts/<audio_url>' .`
  (a similarly-named `dev01-…` bucket exists but holds only stale dev files —
  do not use it).

Recipe: pick a few chapters, download their MP3s, and use each manifest's
segment timings to slice sentence-level clips with ffmpeg, pairing every clip
with its sentence text (match `text_preview` against the chapter content —
previews may be truncated). Resample everything to one rate (16 kHz or
22.05 kHz mono WAV). A couple of chapters already yields hundreds of clips;
keep at least 10 good pairs and hold some out for the benchmark.

**Fallback (only if the database or bucket is truly unreachable after real
attempts):** synthesize ≥10 Vietnamese pairs locally (e.g. macOS
`say -v Linh`) and set `"data_source": "fallback-local-synth"` in the report.
Using the fallback without demonstrated attempts against production is a fail.

## Stage 2 — Design (DESIGN.md)

Write `DESIGN.md`: chosen architecture (backbone vs scratch, with the
justification), parameter budget, audio representation (mel bins / codec),
vocoder choice, why it will hit **RTF < 1.0 on this CPU** (show the working:
params × sample-rate arithmetic, not vibes), and the training recipe you would
run at full scale vs what this smoke run does.

## Stage 3 — Pipeline (must actually run)

- `prepare_data.py` — builds the dataset from Stage 1 files.
- `train.py` — trains your model on the pairs for **≥ 50 optimizer steps** on
  CPU, logs loss, saves a checkpoint. Loss at the end must be lower than at
  the start (it's a smoke run — overfitting a few pairs is fine and expected).
- `infer.py <text> <out.wav>` — loads the checkpoint, synthesizes a WAV on
  CPU. Wire a simple vocoder (Griffin-Lim is acceptable at smoke scale; note
  the production choice in DESIGN.md).

## Stage 4 — Benchmark + report

- `bench.py` — synthesizes **≥ 3 held-out Vietnamese sentences** on CPU,
  measures RTF = synthesis_wall_time / produced_audio_duration per sentence,
  writes at least 3 WAVs into `samples/`.
- `report.json` — exactly this shape (all values measured, not estimated):

```json
{
  "data_source": "prod-thuy-trang" | "fallback-local-synth",
  "pairs": <int>,
  "architecture": "<one line>",
  "params": <int>,
  "sample_rate": <int>,
  "train_steps": <int>,
  "loss_start": <float>,
  "loss_end": <float>,
  "rtf": <float mean>,
  "rtf_sentences": <int>,
  "wav_files": ["samples/….wav", …]
}
```

- `REPORT.md` — what you built, measured RTF table, what full-scale training
  would change, honest limitations.

When every stage is done and `report.json` is written, reply with exactly:
`SHIPPED`.
