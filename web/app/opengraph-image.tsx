import { ImageResponse } from 'next/og';
import { BRAND_NAME, HEADLINE, SUBHEAD } from '@/lib/brand';
import { BRAND_COLORS, svgDataUri, waypointsSvg } from '@/lib/brand-mark';

// The shared card. Concept: **the ray as a vector leaving the frame** — a solid
// origin dot and one hairline that runs off the right edge and visibly does not
// stop. Not a scan, not a chart, not a glow. It is the only thing on the card
// that is not type, and it is doing the same job the headline is.
//
// Flat `--background`, no gradient and no glassmorphism (DESIGN.md bans both),
// and the copy is imported from lib/brand.ts rather than retyped, so a card
// that promises something the door does not say is not a thing that can happen.
export const alt = `${BRAND_NAME} — ${HEADLINE}`;
export const size = { width: 1200, height: 630 };
export const contentType = 'image/png';

export default function OpengraphImage() {
  return new ImageResponse(
    (
      <div
        style={{
          width: '100%',
          height: '100%',
          display: 'flex',
          flexDirection: 'column',
          justifyContent: 'space-between',
          background: BRAND_COLORS.background,
          padding: '72px 80px',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 16 }}>
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              width: 56,
              height: 56,
              borderRadius: 14,
              // The in-app wordmark tile: --primary at 16%. color-mix is a
              // stylesheet function and satori has no stylesheet, so the same
              // colour is written as an alpha hex.
              background: '#22c78629',
            }}
          >
            {/* satori rasterises a plain <img>; next/image has no meaning here. */}
            <img src={svgDataUri(waypointsSvg(32))} alt="" width={32} height={32} />
          </div>
          <div style={{ fontSize: 30, color: BRAND_COLORS.foreground, letterSpacing: -0.2 }}>{BRAND_NAME}</div>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 24, maxWidth: 880 }}>
          <div style={{ fontSize: 74, lineHeight: 1.08, color: BRAND_COLORS.foreground, letterSpacing: -1.8 }}>
            {HEADLINE}
          </div>
          <div style={{ fontSize: 28, lineHeight: 1.45, color: BRAND_COLORS.mutedForeground }}>{SUBHEAD}</div>
        </div>

        {/* The ray. Origin dot, then a 2px rule that runs past the 80px padding
            to x = 1200 — it leaves the frame rather than terminating inside it,
            which is the whole point of drawing it. */}
        <div style={{ display: 'flex', alignItems: 'center', marginRight: -80 }}>
          <div style={{ width: 14, height: 14, borderRadius: 7, background: BRAND_COLORS.primary }} />
          <div style={{ flex: 1, height: 2, background: BRAND_COLORS.primary }} />
        </div>
      </div>
    ),
    size,
  );
}
