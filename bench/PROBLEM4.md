# Thính Du Ký: a pixel idle game for kiem-lai audiobook listeners

Build a **single-file pixel web mini-game** for **kiem-lai**
(truyen.lohi2.com), the Vietnamese novel-reading platform. The product
insight: while a chapter's TTS audio plays, the reader's ears are busy but
their **eyes and one thumb are free**. Give them a quiet little game that
lives beside the audio player — something to fiddle with for hours of
listening without ever demanding attention.

## Market brief (July 2026 — already researched, design to it)

Pure hyper-casual is fading; what is winning in 2026 browsers is the
**"active idle"** hybrid with **pixel art**: games like Microcivilization,
Mr. Mine, and Melvor Idle. The formula:

- **Progress accrues with zero attention** (numbers grow while you only
  listen), but **tapping meaningfully accelerates** it — both postures must
  feel good.
- **Prestige / breakthrough loops**: reset-for-multiplier tiers that make the
  long session (a 20-chapter listening binge) feel like an arc.
- **Readable pixel art**, few colors, chunky sprites — charm without asset
  budgets.
- **Respect the player's time**: offline/away progress is credited on return.

## Theme

kiem-lai readers are deep in **tiên hiệp / kiếm hiệp** novels. Make the game
a **sword-cultivation idle**: a lone pixel cultivator meditates and
accumulates **linh khí**; the player taps to channel **kiếm khí**; realms
break through in the classic ladder (Luyện Khí → Trúc Cơ → Kim Đan →
Nguyên Anh → Hóa Thần → …), each breakthrough a prestige rung. All UI text
**in Vietnamese**. Flavor touches tied to reading/listening (e.g. the
cultivator "nghe đạo" — cultivating by listening — so listening time = qi) are
strongly encouraged.

## Hard requirements

1. **One file: `game.html`.** Self-contained, opens from `file://`, works
   fully offline. **Zero external requests** — no CDN scripts, no external
   fonts/images/styles, no `fetch`/XHR/WebSocket. Procedurally drawn sprites
   only.
2. **Completely silent.** The user is listening to an audiobook: no
   `<audio>`, no `Audio(`, no AudioContext, no autoplay of anything audible.
3. **Pixel rendering:** a `<canvas>` game scene with
   `image-rendering: pixelated` (draw at a small logical resolution and
   scale up), animated via `requestAnimationFrame`.
4. **Active-idle loop:** qi accrues per second untouched; tapping/holding the
   canvas adds a visible burst; at thresholds the player can **Đột phá**
   (breakthrough) to the next realm for a permanent multiplier. At least 5
   named realms. Away progress: persist a timestamp and credit elapsed time
   (capped is fine) on reload, telling the player what they earned.
5. **One-hand, portrait-first:** everything reachable with one thumb on a
   ~390×844 viewport; also fine resized on desktop; no horizontal page
   scroll.
6. **kiem-lai design alignment.** Use the platform's real dark-theme tokens
   for the UI chrome around the canvas (define them as CSS variables with
   exactly these values):
   - `--background: #000000`, `--card: #0a0a0a`, `--foreground: #f5f0e8`
   - `--primary: #ef4444` (accent red), `--muted: #141414`,
     `--muted-foreground: #8a857c`, `--border: rgba(255, 255, 255, 0.06)`
   - `--radius: 0.75rem` for cards/buttons; system-ui/sans font stack.
7. **Persistence:** save to `localStorage` (auto-save at least every few
   seconds and on `visibilitychange`); a small "Chơi lại từ đầu" reset must
   exist but be mis-tap-proof (confirm step).
8. **No frameworks.** Vanilla HTML/CSS/JS in the one file. Keep it under
   150 KB.

## Deliverables (in the workspace root)

- `game.html` — the game.
- `verify.mjs` — a Node script **you write and run** that reads `game.html`
  and asserts the mechanical requirements (single file, no external URLs, no
  audio APIs, canvas + pixelated + rAF present, localStorage save, the six
  token values present, Vietnamese realm names present, file size < 150 KB).
  It must exit 0 on success and print one line per check. `node` is on PATH.
- `README.md` — ≤ 1 page: the loop, the numbers (rates, thresholds,
  multipliers), why it fits the 2026 active-idle trend, and how a player
  listens + plays at the same time.

Work iteratively: write the game, run `verify.mjs`, read your own output,
fix, repeat until verification is green. The graders re-run `node verify.mjs`
and also apply their own static checks — a `verify.mjs` that lies fails the
whole run.
