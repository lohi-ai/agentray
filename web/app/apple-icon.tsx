import { ImageResponse } from 'next/og';
import { BRAND_COLORS, svgDataUri, waypointsSvg } from '@/lib/brand-mark';

// Apple's touch icon must be a raster, so it is generated rather than checked
// in — same glyph and same two colours as app/icon.svg, no binary asset in the
// repo, and no way for the two to drift apart in the ways that matter.
export const size = { width: 180, height: 180 };
export const contentType = 'image/png';

export default function AppleIcon() {
  return new ImageResponse(
    (
      <div
        style={{
          width: '100%',
          height: '100%',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          background: BRAND_COLORS.background,
        }}
      >
        {/* satori rasterises a plain <img>; next/image has no meaning here. */}
        <img src={svgDataUri(waypointsSvg(112))} alt="" width={112} height={112} />
      </div>
    ),
    size,
  );
}
