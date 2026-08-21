/**
 * Which app an event came from.
 *
 * The server can usually infer this from the user agent, but inference is a
 * fallback, not the contract: `classifyPlatform` on the ingest side takes an
 * explicit `platform` property ahead of every heuristic, precisely so a client
 * that knows what it is never has to be guessed at. A browser that stays silent
 * is classified by its `Mozilla/5.0` UA and gets the right answer today — until
 * it runs somewhere that is not a plain tab (an Electron renderer, a Capacitor
 * shell, a prerender bot, a jsdom test) and quietly lands in the wrong bucket,
 * or in "unknown".
 *
 * So every SDK states its platform. Override it when this bundle is not the
 * website: a Capacitor/Cordova build inside the iOS app should say `ios`, or its
 * users show up as web traffic in a product that also has a real website.
 */
export const DEFAULT_PLATFORM = 'web';

/**
 * Stamps the platform onto a caller's properties.
 *
 * Applied *after* the caller's own keys so an app cannot mislabel which product
 * surface an event came from by accident — the same ordering the Swift client
 * uses. Someone who genuinely needs a different value sets it once at `init()`.
 */
export function withPlatform(
  properties: Record<string, unknown>,
  platform: string,
): Record<string, unknown> {
  return { ...properties, platform };
}
