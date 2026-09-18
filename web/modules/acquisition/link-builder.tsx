'use client';

import { useState } from 'react';
import { Check, Copy, Link as LinkIcon, RotateCcw } from 'lucide-react';
import { Button, Panel } from '@/modules/shared/components/signal-primitives';

const inputCls =
  'h-9 w-full rounded-md border border-[var(--color-border-emphasized)] bg-[var(--color-background-muted)] px-3 text-sm text-[var(--color-text-primary)] outline-none focus:border-primary focus:shadow-[0_0_0_3px_var(--ring)] placeholder:text-[var(--color-text-secondary)]';
const labelCls = 'mb-1.5 block text-xs font-medium text-[var(--color-text-secondary)]';


export interface UTMParams {
  source?: string;
  medium?: string;
  campaign?: string;
  term?: string;
  content?: string;
}

export function buildUTMURL(url: string, params: UTMParams): string {
  const trimmedUrl = url.trim();
  if (!trimmedUrl) return '';

  const entries: Array<[string, string]> = [];
  if (params.source?.trim()) entries.push(['utm_source', params.source.trim()]);
  if (params.medium?.trim()) entries.push(['utm_medium', params.medium.trim()]);
  if (params.campaign?.trim()) entries.push(['utm_campaign', params.campaign.trim()]);
  if (params.term?.trim()) entries.push(['utm_term', params.term.trim()]);
  if (params.content?.trim()) entries.push(['utm_content', params.content.trim()]);

  if (entries.length === 0) return trimmedUrl;
  try {
    if (!trimmedUrl.startsWith('/') && (trimmedUrl.includes('://') || trimmedUrl.includes('.'))) {
      const base = trimmedUrl.includes('://') ? trimmedUrl : `https://${trimmedUrl}`;
      const parsed = new URL(base);
      for (const [k, v] of entries) {
        parsed.searchParams.set(k, v);
      }
      return trimmedUrl.includes('://') ? parsed.toString() : parsed.toString().replace(/^https:\/\//, '');
    }
  } catch {
    // Fallback for custom schemes or unparseable URLs
  }
  const [pathAndQuery, hash] = trimmedUrl.split('#', 2);
  const sep = pathAndQuery.includes('?') ? '&' : '?';
  const search = entries.map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`).join('&');
  return `${pathAndQuery}${sep}${search}${hash ? `#${hash}` : ''}`;
}
export function UTMLinkBuilder() {
  const [url, setUrl] = useState('');
  const [source, setSource] = useState('');
  const [medium, setMedium] = useState('');
  const [campaign, setCampaign] = useState('');
  const [term, setTerm] = useState('');
  const [content, setContent] = useState('');
  const [copied, setCopied] = useState(false);

  const taggedURL = buildUTMURL(url, { source, medium, campaign, term, content });

  const handleCopy = async () => {
    if (!taggedURL) return;
    try {
      await navigator.clipboard.writeText(taggedURL);
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    } catch {
      // Clipboard denied; user can still select from the readonly input.
    }
  };

  const handleReset = () => {
    setUrl('');
    setSource('');
    setMedium('');
    setCampaign('');
    setTerm('');
    setContent('');
  };

  const hasAnyParam = Boolean(source || medium || campaign || term || content);

  return (
    <Panel
      title="Campaign link builder"
      action={
        hasAnyParam || url ? (
          <Button variant="ghost" size="sm" icon={<RotateCcw size={13} />} onClick={handleReset}>
            Reset
          </Button>
        ) : undefined
      }
    >
      <div className="flex flex-col gap-4">
        <p className="text-sm text-[var(--color-text-secondary)]">
          Tag campaign links with UTM parameters so AgentRay attributes visits automatically.
          The browser SDK captures all five parameters on pageviews, and acquisition ranks them
          over referrer channels.
        </p>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
          <div className="sm:col-span-2 lg:col-span-3">
            <label className={labelCls} htmlFor="utm-builder-url">
              Website URL <span className="font-normal text-[var(--color-text-secondary)] opacity-75">(landing page)</span>
            </label>
            <input
              id="utm-builder-url"
              className={inputCls}
              placeholder="https://example.com/landing"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          </div>

          <div>
            <label className={labelCls} htmlFor="utm-builder-source">
              Campaign source <span className="text-[var(--danger)]">*</span>{' '}
              <span className="font-mono text-xs opacity-75">(utm_source)</span>
            </label>
            <input
              id="utm-builder-source"
              className={inputCls}
              placeholder="e.g. newsletter, google, x"
              value={source}
              onChange={(e) => setSource(e.target.value)}
            />
          </div>

          <div>
            <label className={labelCls} htmlFor="utm-builder-medium">
              Campaign medium <span className="font-mono text-xs opacity-75">(utm_medium)</span>
            </label>
            <input
              id="utm-builder-medium"
              className={inputCls}
              placeholder="e.g. email, cpc, social"
              value={medium}
              onChange={(e) => setMedium(e.target.value)}
            />
          </div>

          <div>
            <label className={labelCls} htmlFor="utm-builder-campaign">
              Campaign name <span className="font-mono text-xs opacity-75">(utm_campaign)</span>
            </label>
            <input
              id="utm-builder-campaign"
              className={inputCls}
              placeholder="e.g. launch-week, spring-sale"
              value={campaign}
              onChange={(e) => setCampaign(e.target.value)}
            />
          </div>

          <div>
            <label className={labelCls} htmlFor="utm-builder-term">
              Campaign term <span className="font-mono text-xs opacity-75">(utm_term)</span>
            </label>
            <input
              id="utm-builder-term"
              className={inputCls}
              placeholder="e.g. paid search keyword"
              value={term}
              onChange={(e) => setTerm(e.target.value)}
            />
          </div>

          <div>
            <label className={labelCls} htmlFor="utm-builder-content">
              Campaign content <span className="font-mono text-xs opacity-75">(utm_content)</span>
            </label>
            <input
              id="utm-builder-content"
              className={inputCls}
              placeholder="e.g. hero-cta, banner-v2"
              value={content}
              onChange={(e) => setContent(e.target.value)}
            />
          </div>
        </div>

        <div className="mt-1 flex flex-col gap-2">
          <label className={labelCls} htmlFor="utm-builder-result">
            Tagged URL
          </label>
          <div className="flex items-center gap-2">
            <div className="relative flex-1">
              <input
                id="utm-builder-result"
                readOnly
                value={taggedURL}
                placeholder="Fill in the fields above to generate your tagged link…"
                className={`${inputCls} font-mono text-xs`}
                onFocus={(e) => e.currentTarget.select()}
              />
            </div>
            <Button
              variant="outline"
              size="sm"
              icon={copied ? <Check size={13} /> : <Copy size={13} />}
              onClick={() => void handleCopy()}
              disabled={!taggedURL}
            >
              {copied ? 'Copied' : 'Copy'}
            </Button>
          </div>
        </div>
      </div>
    </Panel>
  );
}
