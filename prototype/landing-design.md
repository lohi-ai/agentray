# AgentRay landing page — design spec

**Surface:** `/` for a visitor with no session — today `AuthGate` → `AuthScreen`
(`web/modules/shared/auth-gate.tsx`, `auth-screen.tsx`, `auth-value.tsx`).
**Prototype:** `agentray/prototype/landing.html` (open directly in a browser).
**Reference style:** Apple product page — scroll-driven, product-as-image, big
restrained type. **Design contract:** `agentray/DESIGN.md` + `web/lib/brand.ts`.

---

## 1. User and job

A stranger who followed a link to an AgentRay instance (hosted or self-hosted).
`/` is the **only indexable URL** on the site (`lib/brand.ts` `INDEXABLE_PATHS`,
`app/robots.ts`), so this page is simultaneously the pitch, the SEO surface, and
the signup form. Their job: understand in ten seconds what happens *without
them*, then create a workspace. Secondary: a returning user reaching Log in.

## 2. Where Apple's grammar applies, and where AgentRay's overrides it

`DESIGN.md` bans "decorative gradients, glassmorphism, emoji icons, oversized
heroes, and generic marketing-card grids" — that paragraph governs the
**cockpit** ("the screen should feel operational, not theatrical"), while its
principles list says "marketing surfaces sell the next value moment". The split
this design takes:

| Taken from Apple | Refused |
|---|---|
| One idea per screenful; section gap larger than anything inside it | Gradient meshes, glass, glow |
| Very large, tight, low-contrast-of-weight display type | Type that is loud rather than large |
| Product **is** the image; it rises into place on scroll | Parallax, scroll-jacking, decorative motion |
| A sticky visual that changes as the copy scrolls past it | Fake device chrome, stock photography |
| Quiet persistent bar with one action | A second CTA competing with the hero's |

The "product imagery" is **the shipped product's own surfaces** — chat bubble,
tool-call row, funnel bars, channel rows — exactly the technique `auth-value.tsx`
already uses. Nothing on the page is a drawing of a product that does not exist,
and no image asset is introduced (`web/public/` stays empty).

## 3. Copy — all of it already exists

Every promise string comes from `lib/brand.ts` (`HEADLINE`, `SUBHEAD`, `CLAIMS`),
whose `PROMISE_BANNED_VERBS` list is enforced by `brand.test.ts`. Section copy
that is not in `brand.ts` is lifted from shipped sources:

| Where | Source |
|---|---|
| Hero headline / subhead | `HEADLINE`, `SUBHEAD` |
| Loop steps 02 / 03 / 04 titles + details | `CLAIMS[]` verbatim |
| Contrast line 1 | `README.md:5` "Every analytics tool can tell you *what happened*" |
| Contrast line 2 | `README.md:7` "AgentRay ships that loop as the product" |
| Answer mock text | `writtenOpinion()` in `web/lib/ia.ts` |
| Channels section | `/operations` screen canon in `DESIGN.md` |
| Self-host section | `README.md` lines 12–19 |

**Standing bans that survive:** no price, no trial, no customer logo, no
testimonial, no demo promise, and **no surface explains the name** — this
instance may be self-hosted, where the first five would be lies.

## 4. Section anatomy

| # | Section | Content | Product surface |
|---|---|---|---|
| 0 | Sticky bar | mark + wordmark; Log in (ghost) + Create workspace (primary), both anchor `#get-started` | — |
| 1 | Hero | `HEADLINE` as **h1**, `SUBHEAD`, two CTAs | the answer bubble, rising into place |
| 2 | Contrast | two display lines arriving one after the other on one screenful | none — type only |
| 3 | **The loop** | 4 steps: measure / diagnose / test / learn | sticky stage swapping per step: event catalog → funnel with the gap → test brief → memory timeline |
| 4 | Without you | "Nobody has to open the chat." | channel rows (schedules, webhook, chat) with health |
| 5 | Open source | `docker compose up` + three facts | none — three cards |
| 6 | `#get-started` | the auth Card, behaviour unchanged | — |

Section 3 is the centrepiece and the request's core interaction. Sections 1→4
each show a **different** surface; nothing repeats.

## 5. Motion contract

- **Scroll reveals** are CSS scroll-driven animations (`animation-timeline:
  view()` / `scroll(root)`) inside `@supports` + `@media (prefers-reduced-motion:
  no-preference)`. Every animated element's **unanimated state is its final
  state**, so Safari and Firefox — which do not ship scroll timelines yet — get a
  complete, readable page with no JS fallback and no layout thrash. Verified:
  under `--force-prefers-reduced-motion`, 8/8 animated elements compute
  `opacity: 1` and `scroll-behavior: auto`.
- **Step activation** in section 3 is `IntersectionObserver`
  (`rootMargin: -48% 0 -48%`), not a scroll timeline, so the narrative works in
  every browser. The sticky panel is **swapped, never crossfaded**.
- Easing is the app's own `--ease`; nothing discrete exceeds `--duration-medium`.
- No scroll-jacking: the page never takes over the wheel or the scrollbar.

## 6. Accessibility decisions

- **One h1** — the hero headline. This moves the h1 out of the auth card, where
  `auth-screen.tsx` put it; "Create your workspace" becomes an h3 under the
  section's h2. `auth-value.tsx`'s "no Heading in this column" comment is
  obsolete once the page has a real document outline.
- **Every product stage is `aria-hidden`.** It illustrates an answer; a screen
  reader reading a fabricated funnel as this workspace's data is worse than
  silence. Nothing inside is focusable.
- **Inactive scroll steps are NOT dimmed.** Apple fades out-of-focus copy; at any
  opacity low enough to read as "off", `--muted-foreground` body text drops
  under 4.5:1 (`ux-check` accessibility #36). The narrative is carried by the
  rail tick and the step index turning `--primary` instead. This is the one place
  the checklist overrides the reference style.
- `--faint` (#5C6678, ~3.4:1) is used only for rules and ticks, never for copy.
- Skip link; `scroll-margin-top: 76px` on every anchor target so the 56px sticky
  bar never covers a heading; `:focus-visible` ring on `--primary`; all targets
  ≥ 44px; mobile-first (the stacked layout is the base, sticky is the `≥900px`
  enhancement).
- Form: visible labels + `(required)`, `role="alert"` on the banner and the field
  error, `aria-invalid`, correct `autocomplete`, `type="email"`.
- Verified at 390px: **no horizontal overflow** (`scrollWidth === clientWidth`).

## 7. Component mapping

Reused, no changes: `Text` (`display-1/2/3`, `body`, `supporting`, `label`),
`Heading`, `Button` (primary / outline / ghost, `size="lg"`), `Card`, `Section`,
`VStack` / `HStack` / `Grid`, `Divider`, `Badge`, `TextInput`, `Banner`,
`Avatar`, `StatusDot`, `ChatMessage` / `ChatMessageBubble` / `ChatToolCalls`,
`Markdown` — every one of these is already on the door today.

**`NEW:` flags** — three components and one token group, each a decision to
approve rather than an accident:

- `NEW: LandingStage` — the framed holder every product surface sits in. Astryx
  `Card` cannot bleed a surface to its own edge, and six sections need the same
  frame.
- `NEW: ScrollSteps` — the sticky scrollytelling controller for section 3. No
  Astryx component pins a visual against scrolling text; this is the requested
  interaction.
- `NEW: LandingNav` — a marketing top bar. Astryx `TopNav` is app chrome with
  navigation state; the door has no app behind it.
- `NEW: --landing-d1/d2/d3/--landing-lead` — four fluid display steps. The app
  type scale stops at 18px because it is a cockpit scale and a marketing headline
  has no step in it. **Landing-scoped only — these never enter `globals.css`'s
  `:root`.**

Everything else is a token that already exists.

## 8. What must not change in the port

`AuthScreen`'s behaviour is load-bearing and stays identical: sign-up-first mode
default, `validateAuthForm` as the only validator (`noValidate`), inline
`TextInput` status, the API error `Banner`, `HIDDEN_PROJECT_NAME`, the
`pending` disable while `me()` is in flight, the ghost-Button mode toggle, and
the shared element across the unresolved → signed-out transition so typing
survives. The returning-user splash in `auth-gate.tsx` is untouched.

## 9. Non-happy-path states (prototype appendix, section 7)

Rendered in the prototype so a reviewer can judge them in place: session-check
splash, submitting / `pending`-disabled button, log-in mode (two fields, no
workspace name), API-unreachable banner, and a field-level validation error.

## 10. Open decisions for the owner

1. **`#get-started` vs. hero form.** The form sits at the bottom, reached by both
   bar CTAs. The alternative — form in the hero, as today — converts a
   ready-to-sign-up visitor one scroll sooner but spends the hero on a form.
   Recommendation: keep it at the bottom; the bar CTA is one click from anywhere.
2. **Password show/hide** was added to the prototype (ux-check forms #60). It is
   not on the door today. Cheap and low-risk, but it does touch the auth form.
