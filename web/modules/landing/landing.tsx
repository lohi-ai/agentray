'use client';

import type { ReactNode } from 'react';
import { Waypoints } from 'lucide-react';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Heading, Text } from '@astryxdesign/core/Text';
import { BRAND_NAME, HEADLINE, SUBHEAD } from '@/lib/brand';
import type { AuthMode } from '@/lib/auth-form';
import {
  CHANNELS_SECTION,
  CONTRAST,
  DOOR_SECTION,
  OSS_FACTS,
  OSS_SECTION,
} from '@/modules/landing/content';
import { ScrollSteps } from '@/modules/landing/scroll-steps';
import { AnswerStage, ChannelsStage } from '@/modules/landing/stages';
import './landing.css';

// The signed-out door. This is the ONLY indexable URL on the instance
// (lib/brand.ts INDEXABLE_PATHS, app/robots.ts), so it is simultaneously the
// pitch, the SEO surface and the signup form — which is why the form still
// lives on it rather than behind a /signup route a crawler would never see.
//
// Style note, because it looks like a deviation and is not: DESIGN.md bans
// "oversized heroes" and "decorative gradients" and asks for a screen that
// feels "operational, not theatrical". That paragraph governs the *cockpit* —
// its own principles list says marketing surfaces sell the next value moment.
// What is taken from the reference style here is the RESTRAINT: one idea per
// screenful, a section gap larger than anything inside it, very large but very
// quiet type, and scroll used to reveal the product rather than to decorate.
// What is refused is the theatre: no gradient mesh, no glass, no glow, no
// parallax, no scroll-jacking, no stock imagery, no logo wall.
//
// The "product photography" is the product: every stage in stages.tsx is built
// from the same Astryx components the shipped screens use. `web/public/` is
// still empty and no image asset was added.
//
// Copy rules, unchanged from lib/brand.ts and enforced by brand.test.ts: no
// price, no trial, no customer logo, no testimonial, no demo promise, and no
// surface explains the name — the mark carries it (lucide `Waypoints`, a routed
// path with a direction, never a lens or an eye).

// An in-page jump is one of the few places smooth scrolling genuinely helps —
// it shows the reader that they moved within one page rather than navigated
// away. It is also exactly the animation prefers-reduced-motion exists for, so
// it is asked for rather than assumed. This is read per click, not cached, so a
// reader who changes the OS setting mid-session is honoured.
function scrollBehavior(): ScrollBehavior {
  if (typeof window === 'undefined') return 'auto';
  return window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth';
}

export function Landing({
  mode,
  onPickMode,
  form,
}: {
  mode: AuthMode;
  onPickMode: (next: AuthMode) => void;
  form: ReactNode;
}) {
  // Both bar actions land on the same section. They differ only in which mode
  // the form should be in when the reader gets there — a returning user who
  // clicks "Log in" must not arrive at a signup form. onPickMode is called
  // unconditionally so it can also place the caret when the mode already
  // matches.
  const scrollTo = (id: string) => {
    document.getElementById(id)?.scrollIntoView({ behavior: scrollBehavior(), block: 'start' });
  };

  const goToForm = (next: AuthMode) => () => {
    scrollTo('get-started');
    onPickMode(next);
  };

  return (
    <div className="lp-root">
      {/* The same skip link the app shell uses (globals.css .skip-to-content).
          The bar is short, but a keyboard reader still tabs three controls
          before reaching the page on every load. */}
      <a href="#main-content" className="skip-to-content">Skip to content</a>

      <header className="lp-bar">
        <HStack gap={3} align="center" justify="between" className="lp-wrap lp-bar-inner">
          <HStack gap={2} align="center">
            <span className="lp-mark" aria-hidden>
              <Waypoints size={18} />
            </span>
            <Text type="body" weight="semibold">
              {BRAND_NAME}
            </Text>
          </HStack>
          <HStack gap={1} align="center" className="flex-nowrap">
            <Button variant="ghost" size="lg" label="Log in" onClick={goToForm('login')} className="min-h-11" />
            <Button
              variant="primary"
              size="lg"
              label="Create workspace"
              onClick={goToForm('signup')}
              className="lp-bar-signup min-h-11"
            />
          </HStack>
        </HStack>
      </header>

      <main id="main-content">
        {/* 1 · hero ------------------------------------------------------- */}
        <section className="lp-wrap lp-hero">
          <VStack gap={6} align="stretch">
            {/* The page's only h1. It used to live inside the auth card, which
                was right when the card was the whole page; now that the door
                has a real document outline, the promise is the h1 and "Create
                your workspace" is a heading inside its section. */}
            <Heading level={1} className="lp-d1" textWrap="balance">
              {HEADLINE}
            </Heading>
            <Text type="body" color="secondary" as="p" className="lp-lead lp-hero-lead" textWrap="pretty">
              {SUBHEAD}
            </Text>
            <HStack gap={3} align="center" justify="center" className="lp-hero-cta flex-wrap">
              <Button
                variant="primary"
                size="lg"
                label="Create your workspace"
                onClick={goToForm('signup')}
                className="min-h-11"
              />
              <Button
                variant="secondary"
                size="lg"
                label="How the loop runs"
                onClick={() => scrollTo('the-loop')}
                className="min-h-11"
              />
            </HStack>
          </VStack>

          <VStack gap={4} align="stretch" className="lp-settle pt-[clamp(40px,7vh,76px)]">
            <AnswerStage />
            <Text type="supporting">Example of an answer — not data from this instance.</Text>
          </VStack>
        </section>

        {/* 2 · contrast --------------------------------------------------- */}
        {/* One screenful, type only. The commodity line arrives first and the
            product line answers it — the whole positioning in two sentences,
            both lifted from the README's own opening argument. */}
        <section className="lp-wrap lp-contrast" aria-labelledby="lp-contrast-h">
          <VStack gap={4} align="start">
            <Heading id="lp-contrast-h" level={2} color="secondary" className="lp-d2 lp-line-a lp-contrast-line">
              {CONTRAST.commodity}
            </Heading>
            <Text as="p" className="lp-d2 lp-line-b lp-contrast-line">
              {CONTRAST.product}
            </Text>
          </VStack>
        </section>

        {/* 3 · the loop --------------------------------------------------- */}
        <section id="the-loop" className="lp-section" aria-labelledby="lp-loop-h">
          <div className="lp-wrap">
            <VStack gap={3} align="start" className="lp-rise pb-[clamp(32px,5vh,64px)]">
              <Text type="supporting" className="uppercase tracking-[0.06em]">
                The loop
              </Text>
              <Heading id="lp-loop-h" level={2} className="lp-d3" textWrap="balance">
                Measure, diagnose, test, learn — shipped as the product.
              </Heading>
            </VStack>
            <ScrollSteps />
          </div>
        </section>

        {/* 4 · runs without a conversation -------------------------------- */}
        <section className="lp-wrap lp-section" aria-labelledby="lp-channels-h">
          <div className="lp-split">
            <VStack gap={3} align="start" className="lp-rise">
              <Text type="supporting" className="uppercase tracking-[0.06em]">
                {CHANNELS_SECTION.label}
              </Text>
              <Heading id="lp-channels-h" level={2} className="lp-d2" textWrap="balance">
                {CHANNELS_SECTION.title}
              </Heading>
              <Text type="body" color="secondary" as="p" className="lp-lead" textWrap="pretty">
                {CHANNELS_SECTION.detail}
              </Text>
            </VStack>
            <div className="lp-rise">
              <ChannelsStage />
            </div>
          </div>
        </section>

        {/* 5 · open source ------------------------------------------------ */}
        <section className="lp-wrap lp-section" aria-labelledby="lp-oss-h">
          <VStack gap={4} align="start" className="lp-rise">
            <Text type="supporting" className="uppercase tracking-[0.06em]">
              {OSS_SECTION.label}
            </Text>
            <Heading id="lp-oss-h" level={2} className="lp-d2" textWrap="balance">
              {OSS_SECTION.title}
            </Heading>
            <HStack gap={3} align="center" className="lp-cmd w-full">
              <Text type="code" color="active" aria-hidden>
                $
              </Text>
              <Text type="code">{OSS_SECTION.command}</Text>
            </HStack>
          </VStack>
          <Grid columns={{ minWidth: 260, max: 3 }} gap={4} className="lp-rise pt-[clamp(28px,4vh,48px)]">
            {OSS_FACTS.map((fact) => (
              <Card key={fact.id} padding={5} height="100%">
                <VStack gap={2} align="start">
                  <Heading level={3} type="display-3" className="text-lg">
                    {fact.title}
                  </Heading>
                  <Text type="supporting">{fact.detail}</Text>
                </VStack>
              </Card>
            ))}
          </Grid>
        </section>

        {/* 6 · get started ------------------------------------------------ */}
        <section id="get-started" className="lp-wrap lp-section" aria-labelledby="lp-door-h">
          <div className="lp-split">
            <VStack gap={4} align="start">
              <Heading id="lp-door-h" level={2} className="lp-d2" textWrap="balance">
                {DOOR_SECTION.title}
                <br />
                {DOOR_SECTION.titleSecondLine}
              </Heading>
              <Text type="body" color="secondary" as="p" className="lp-lead" textWrap="pretty">
                {DOOR_SECTION.detail}
              </Text>
            </VStack>
            {form}
          </div>
        </section>
      </main>

      <footer className="lp-wrap py-6 border-t border-border">
        <HStack gap={3} align="center" justify="between" className="flex-wrap">
          <Text type="supporting">{BRAND_NAME} — open source, self-hostable.</Text>
          <a
            href={OSS_SECTION.repo}
            className="text-sm text-muted-foreground underline underline-offset-4 hover:text-foreground"
          >
            github.com/lohi-ai/agentray
          </a>
        </HStack>
      </footer>
    </div>
  );
}
