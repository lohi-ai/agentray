# Tu Tiên Du Ký v2: an endless chill pixel world inside the kiem-lai reader

You are working **inside the real kiem-lai `web/` Next.js codebase** (your file
tools and shell are rooted there). Ship a production-quality feature: an
**endless side-scrolling pixel world** the reader wanders while listening to a
chapter's TTS audio — the spirit of **Ninja School** (the beloved Vietnamese
pixel RPG: side-view chibi characters, warm chunky sprites, little villages
and bamboo roads), but **chill**: no death, no fail state, no timers, nothing
to lose.

## The vision

A chibi cultivator **auto-walks** through a procedurally generated,
never-ending side-view landscape — bamboo groves, rice paddies, village
gates, temples, mountain passes — with layered parallax scenery. Ears on the
audiobook, eyes drifting to the little world going by. Interaction is
optional sugar: tap to dash/slash a passing mob, pick up linh thạch, greet a
villager. **Linh khí accrues on its own; listening makes it flow faster.**

- **Endless map**: deterministic procedural chunks from a seed stored in the
  save — walking distance persists, and re-walking the same li of road shows
  the same scenery. Scenery/biome palette evolves as the player's realm rises,
  so a long listening binge visibly deepens the world.
- **Chill loop**: idle accrual always; **auto-walk is the default posture**
  (zero attention required); taps give small visible bursts. Đột phá
  (breakthrough) at thresholds is the prestige rung — new realm, new biome
  flavor, permanent multiplier.
- **Realms**: reuse the platform's own ladder — import `REALM_NAMES` from
  `@/lib/hoa-than/identity` (6 realms, Luyện Khí → Đại Thừa). Do not invent a
  parallel ladder.

## Platform integration (the point of v2)

1. **Nghe đạo — listening is cultivation.** Subscribe to the global audio
   player: `import { useAudioPlayer } from '@/lib/stores/audio-player'` then
   `const isPlaying = useAudioPlayer((s) => s.isPlaying)`. While audio is
   actually playing, qi rate gets a strong visible bonus (e.g. ×3) and the HUD
   shows it (the store also exposes `novelTitle` / `chapterNumber` for flavor,
   e.g. "Đang nghe: <novel> — Ch.<n>").
2. **Hoá thân avatar is the wanderer.** If the user has an active persona,
   the character carries its name (and avatar in the HUD):
   `import { getActivePersona } from '@/lib/queries/hoa-than'` →
   `{ persona: { characterId, name, avatarUrl } | null }` (plain async fn —
   call it from an effect, tolerate failure). Auth state comes from
   `import { useAuth } from '@/modules/auth/hooks'` (`isAuthenticated`,
   `hydrated`). Guests and persona-less users get the fallback wanderer
   "Đạo Hữu Vô Danh" — the game must be fully playable logged out.
3. **Game state stays client-side**: `localStorage` save (auto-save every few
   seconds + on `visibilitychange`), away-progress credited on return (capped
   is fine, tell the player what they earned). **No API or backend changes of
   any kind.**

## Codebase conventions (follow exactly)

- **Module pattern** (mirror `modules/agent-login/` + `app/dang-nhap-agent/`):
  put ALL logic in `modules/tu-tien/` — `page.tsx` starting with
  `'use client'` and default-exporting the page component, an `index.ts` with
  `export * from './page'; export { default } from './page';`, plus as many
  supporting files as you need (engine, sprites, chunks, save — keep files
  focused). The route `app/tu-tien/page.tsx` is a thin server re-export:
  `export { default } from '@/modules/tu-tien';` plus `metadata`.
- **Design system**: UI chrome (HUD, buttons, dialogs, headers) uses
  `@lohi-ui` components (`Button`, `Dialog`, `Progress`, `Badge`, `toast`, …)
  and Tailwind token classes (`bg-background`, `text-muted-foreground`,
  `border-border`, `text-brand-500`) — **never a hardcoded color in
  JSX/CSS**. Exception: the pixel-art palette *inside* canvas drawing code is
  yours to choose.
- **Canvas**: draw at a small logical resolution, scale up with
  `image-rendering: pixelated`, animate with `requestAnimationFrame`,
  procedural sprites only (no image assets, no new dependencies).
- **Silent**: no `<audio>`, no `Audio(`, no AudioContext — the platform's
  player owns sound.
- **Mobile-first portrait**, one-thumb friendly, no horizontal page scroll;
  degrade gracefully on desktop widths. Handle the store hooks' hydration
  safety (they are safe-hook wrapped — use them as normal selectors).
- **Entry point**: add exactly ONE link tile to the game (label it clearly,
  e.g. "Tu Tiên Du Ký") in `modules/profile/ProfileContent.tsx`, styled like
  the neighboring tiles (it already links to `/hoa-than` the same way). Do
  NOT touch `BottomNav`/route-rules.

## Scope fence (hard)

You may create/modify files ONLY under `modules/tu-tien/`, `app/tu-tien/`,
and the single tile edit in `modules/profile/ProfileContent.tsx`. No new
npm dependencies, no `package.json`/lockfile changes, no edits to shared
components/stores, no git commits — leave everything as uncommitted working
tree changes for human review.

## Definition of done

- `pnpm run lint` exits 0 (warnings fine, errors not) — run it.
- `pnpm exec tsc --noEmit` exits 0 — run it.
- `modules/tu-tien/README.md` (≤ 1 page): the loop and numbers, the chunk
  generation scheme, the integration seams used, and what a v3 with a backend
  (leaderboard/PvP) would add.
- The graders re-run lint + tsc themselves and diff the working tree against
  the scope fence — out-of-fence edits fail the run.

Work iteratively: build, run lint/tsc via the shell, read the errors, fix.
