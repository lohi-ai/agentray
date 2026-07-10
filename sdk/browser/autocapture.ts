/**
 * AgentRay browser autocapture — framework-agnostic, dependency-free.
 *
 * Install once per app and every page becomes observable without per-button
 * wiring. The SDK never talks to the network itself: it emits through the
 * `capture(event, properties)` function you pass in, so it plugs into any
 * client (AgentRay, PostHog-compatible, or a typed-emitter sink).
 *
 * What it tracks:
 *  - Pageviews   → `user.pageview` { path, $referrer, title } on load and on
 *                  every SPA navigation (history.pushState/replaceState/popstate),
 *                  deduped by path. Powers AgentRay's Web analytics tab.
 *  - Clicks      → `$autocapture` { tag, label, href, path } via one delegated
 *                  listener on links, buttons, role="button", submit inputs,
 *                  summary, and anything with [data-track]. Opt out per subtree
 *                  with [data-track-ignore].
 *  - Element views → `element_viewed` { label, path } the first time an element
 *                  marked [data-track-view="label"] is at least half visible.
 *                  New elements are picked up automatically (MutationObserver).
 *
 * Returns an uninstall function that removes every listener/observer and
 * restores the patched history methods.
 */

export type AutocaptureCapture = (
  event: string,
  properties: Record<string, unknown>,
) => void;

export interface AutocaptureOptions {
  /** Emit `user.pageview` on load + SPA navigations. Default true. */
  pageviews?: boolean;
  /** Emit `$autocapture` for delegated clicks. Default true. */
  clicks?: boolean;
  /** Emit `element_viewed` for [data-track-view] elements. Default true. */
  elementViews?: boolean;
}

export interface CaptureConfig {
  /**
   * CSS selector allowlist for click capture. Only clicks on elements
   * matching this selector are emitted. Defaults to the built-in catch-all
   * (links, buttons, [data-track], etc.) when omitted.
   */
  clickAllowlist?: string;
  /**
   * Hostnames treated as internal to the product. Referrers from these
   * hosts are normalized to empty string so they don't appear as external
   * traffic in the referrer table.
   */
  internalHosts?: string[];
}

const CLICK_SELECTOR =
  'a,button,[role="button"],input[type="submit"],input[type="button"],summary,[data-track]';

function normalizeReferrer(referrer: string, internalHosts: string[]): string {
  if (!referrer || internalHosts.length === 0) return referrer;
  try {
    const host = new URL(referrer).hostname;
    if (internalHosts.some((h) => host === h || host.endsWith('.' + h))) return '';
  } catch {
    return '';
  }
  return referrer;
}

const MAX_LABEL_LENGTH = 80;

function elementLabel(el: Element): string {
  const explicit = el.getAttribute('data-track') || el.getAttribute('aria-label');
  if (explicit) return explicit;
  const text = (el.textContent || '').replace(/\s+/g, ' ').trim();
  if (text) return text.slice(0, MAX_LABEL_LENGTH);
  return el.id || '';
}

export function installAutocapture(
  capture: AutocaptureCapture,
  options: AutocaptureOptions = {},
  config: CaptureConfig = {},
): () => void {
  if (typeof window === 'undefined' || typeof document === 'undefined') {
    return () => {};
  }

  const { pageviews = true, clicks = true, elementViews = true } = options;
  const { clickAllowlist, internalHosts = [] } = config;
  const clickSelector = clickAllowlist || CLICK_SELECTOR;
  const teardowns: Array<() => void> = [];

  // ── Pageviews ────────────────────────────────────────────────────────────
  if (pageviews) {
    let lastPath = '';
    let lastUrl = document.referrer;

    const emitPageview = () => {
      const path = location.pathname;
      if (path === lastPath) return;
      lastPath = path;
      capture('user.pageview', {
        path,
        $referrer: normalizeReferrer(lastUrl, internalHosts),
        title: document.title,
      });
      lastUrl = location.href;
    };

    const originalPushState = history.pushState.bind(history);
    const originalReplaceState = history.replaceState.bind(history);
    history.pushState = (...args: Parameters<History['pushState']>) => {
      originalPushState(...args);
      emitPageview();
    };
    history.replaceState = (...args: Parameters<History['replaceState']>) => {
      originalReplaceState(...args);
      emitPageview();
    };
    window.addEventListener('popstate', emitPageview);
    teardowns.push(() => {
      history.pushState = originalPushState;
      history.replaceState = originalReplaceState;
      window.removeEventListener('popstate', emitPageview);
    });

    emitPageview();
  }

  // ── Clicks ───────────────────────────────────────────────────────────────
  if (clicks) {
    const onClick = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Element)) return;
      const el = target.closest(clickSelector);
      if (!el || el.closest('[data-track-ignore]')) return;
      capture('$autocapture', {
        tag: el.tagName.toLowerCase(),
        label: elementLabel(el),
        href: el instanceof HTMLAnchorElement ? el.href : undefined,
        path: location.pathname,
      });
    };
    document.addEventListener('click', onClick, true);
    teardowns.push(() => document.removeEventListener('click', onClick, true));
  }

  // ── Element views ────────────────────────────────────────────────────────
  if (elementViews && 'IntersectionObserver' in window) {
    const seen = new WeakSet<Element>();
    const viewObserver = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting || seen.has(entry.target)) continue;
          seen.add(entry.target);
          viewObserver.unobserve(entry.target);
          capture('element_viewed', {
            label:
              entry.target.getAttribute('data-track-view') ||
              elementLabel(entry.target),
            path: location.pathname,
          });
        }
      },
      { threshold: 0.5 },
    );

    const observeWithin = (root: Element) => {
      if (root.matches('[data-track-view]') && !seen.has(root)) {
        viewObserver.observe(root);
      }
      root.querySelectorAll('[data-track-view]').forEach((el) => {
        if (!seen.has(el)) viewObserver.observe(el);
      });
    };

    const mutationObserver = new MutationObserver((mutations) => {
      for (const mutation of mutations) {
        mutation.addedNodes.forEach((node) => {
          if (node instanceof Element) observeWithin(node);
        });
      }
    });

    const start = () => {
      observeWithin(document.documentElement);
      mutationObserver.observe(document.body, { childList: true, subtree: true });
    };
    if (document.body) {
      start();
    } else {
      document.addEventListener('DOMContentLoaded', start, { once: true });
    }

    teardowns.push(() => {
      viewObserver.disconnect();
      mutationObserver.disconnect();
    });
  }

  return () => {
    for (const teardown of teardowns) teardown();
  };
}
