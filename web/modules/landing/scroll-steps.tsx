'use client';

import { useEffect, useRef, useState } from 'react';
import { VStack } from '@astryxdesign/core/Stack';
import { Heading, Text } from '@astryxdesign/core/Text';
import { LOOP_STEPS } from '@/modules/landing/content';
import { LOOP_STAGES } from '@/modules/landing/stages';

// NEW: ScrollSteps — the sticky scrollytelling section. No Astryx component
// pins a visual against scrolling text, and this is the page's core
// interaction, so it is the one bespoke layout on the door.
//
// Two rules it is built to:
//
//  1. **The steps are the content.** They are a real ordered list, in DOM
//     order, fully readable with no JavaScript, no sticky positioning and no
//     scroll timeline. The sticky panel is decorative (`aria-hidden`) and only
//     exists at ≥900px. Below that the same stage renders inline under its own
//     step — one source (`LOOP_STAGES`), two renderings, so the phone and the
//     desktop can never drift.
//  2. **Activation is IntersectionObserver, not a scroll timeline.** The CSS in
//     landing.css degrades to a static page in browsers without
//     `animation-timeline`; the *narrative* must not, so the part that carries
//     meaning uses the API every browser ships. The panel is swapped, never
//     crossfaded, which is also what makes this correct under
//     prefers-reduced-motion with no extra branch.
export function ScrollSteps() {
  const [active, setActive] = useState(0);
  const stepsRef = useRef<Array<HTMLLIElement | null>>([]);

  useEffect(() => {
    const nodes = stepsRef.current.filter((node): node is HTMLLIElement => node != null);
    if (nodes.length === 0) return;

    // A 4%-tall band across the middle of the viewport: a step becomes active
    // when it crosses the centre, which is where the reader's eye is. Anything
    // wider makes two steps active at once on a short viewport.
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          const index = Number((entry.target as HTMLElement).dataset.step);
          if (Number.isInteger(index)) setActive(index);
        }
      },
      { rootMargin: '-48% 0px -48% 0px', threshold: 0 },
    );

    for (const node of nodes) observer.observe(node);
    return () => observer.disconnect();
  }, []);

  return (
    <div className="lp-loop-grid">
      <ol className="lp-steps">
        {LOOP_STEPS.map((step, index) => {
          const Stage = LOOP_STAGES[step.id];
          const isActive = index === active;
          return (
            <li
              key={step.id}
              ref={(node) => {
                stepsRef.current[index] = node;
              }}
              className="lp-step"
              data-step={index}
              data-active={isActive}
            >
              <div className="lp-step-inner">
                <span className="lp-rail" aria-hidden />
                <VStack gap={3} align="start" className="min-w-0">
                  <Text
                    type="code"
                    color={isActive ? 'active' : 'secondary'}
                    className="tracking-[0.08em] uppercase"
                  >
                    {step.index} / {step.phase}
                  </Text>
                  <Heading level={3} className="lp-d3" textWrap="balance">
                    {step.title}
                  </Heading>
                  <Text type="body" color="secondary" className="lp-lead" textWrap="pretty">
                    {step.detail}
                  </Text>
                  {/* Below 900px this is the step's own visual; above it, CSS
                      hides it and the sticky panel takes over. */}
                  <div className="lp-step-visual w-full pt-2">
                    <Stage />
                  </div>
                </VStack>
              </div>
            </li>
          );
        })}
      </ol>

      <div className="lp-sticky" aria-hidden>
        {LOOP_STEPS.map((step, index) => {
          const Stage = LOOP_STAGES[step.id];
          return (
            <div key={step.id} className="lp-panel" data-active={index === active}>
              <Stage />
            </div>
          );
        })}
      </div>
    </div>
  );
}
