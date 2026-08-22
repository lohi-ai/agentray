// The Waypoints glyph as a standalone SVG string, for the two generated images
// (app/apple-icon.tsx, app/opengraph-image.tsx). Satori — what `next/og` renders
// with — does not draw SVG primitives from JSX, but it does rasterise an <img>
// whose src is an SVG data URI, so this is how the real mark reaches a PNG
// instead of being redrawn as a pile of divs that drifts from the app.
//
// Path data is verbatim lucide-react 1.18.0 (icons/waypoints.mjs), the same
// glyph modules/shared/auth-value.tsx renders. Kept in sync by hand with
// app/icon.svg, which is a static file and cannot import this.

// Compiled values of `--primary` / `--background` / `--foreground` /
// `--muted-foreground` from app/globals.css. An OG image is rendered by a
// crawler-side rasteriser with no stylesheet, so the tokens cannot be
// referenced — this is the second and last place that is true. Change a token,
// change these.
export const BRAND_COLORS = {
  primary: '#22c786',
  background: '#0A0E12',
  foreground: '#E9EEF4',
  mutedForeground: '#8C97A8',
} as const;

export function waypointsSvg(size: number, stroke: string = BRAND_COLORS.primary): string {
  return [
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="${size}" height="${size}"`,
    ` fill="none" stroke="${stroke}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">`,
    '<path d="m10.586 5.414-5.172 5.172"/>',
    '<path d="m18.586 13.414-5.172 5.172"/>',
    '<path d="M6 12h12"/>',
    '<circle cx="12" cy="20" r="2"/>',
    '<circle cx="12" cy="4" r="2"/>',
    '<circle cx="20" cy="12" r="2"/>',
    '<circle cx="4" cy="12" r="2"/>',
    '</svg>',
  ].join('');
}

export function svgDataUri(svg: string): string {
  return `data:image/svg+xml;base64,${Buffer.from(svg).toString('base64')}`;
}
