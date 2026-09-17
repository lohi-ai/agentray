'use client';

import { useEffect, useRef, useState } from 'react';
import { ExternalLink, KeyRound, Loader2 } from 'lucide-react';
import { Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import { AgentRayAPI, type WorkspaceProvider } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';
import { Button } from '@/modules/shared/components/signal-primitives';
import { vendorLabel } from './provider-form';

// OAuthSignIn is the "add an account" flow for subscription vendors
// (claude-code, openai-codex, google-antigravity). The vendors' OAuth clients
// only allow localhost redirect URIs, so there is no server callback: the user
// signs in on the vendor's own page, the browser lands on a localhost URL that
// never loads, and they paste that URL (or the bare code) back here. Codex
// additionally offers its device-code flow — no paste at all.
export function OAuthSignIn({
  provider,
  onDone,
  onCancel,
}: {
  provider: WorkspaceProvider;
  onDone: () => void;
  onCancel: () => void;
}) {
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);
  const setMessage = useUIStore((s) => s.setMessage);
  const api = () => new AgentRayAPI(projectID!);

  const [state, setState] = useState('');
  const [authURL, setAuthURL] = useState('');
  const [pasted, setPasted] = useState('');
  const [starting, setStarting] = useState(false);
  const [completing, setCompleting] = useState(false);
  const [device, setDevice] = useState<{ pendingID: string; userCode: string; url: string } | null>(null);
  const [deviceStatus, setDeviceStatus] = useState<'idle' | 'waiting' | 'done' | 'error'>('idle');
  const pollTimer = useRef<number | null>(null);
  const isCodex = provider.vendor === 'openai-codex';

  useEffect(() => () => {
    if (pollTimer.current) window.clearTimeout(pollTimer.current);
  }, []);

  const start = async () => {
    setStarting(true);
    try {
      const res = await api().startProviderOAuth(provider.id);
      setState(res.state);
      setAuthURL(res.auth_url);
      window.open(res.auth_url, '_blank', 'noopener,noreferrer');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not start sign-in');
    } finally {
      setStarting(false);
    }
  };

  const complete = async () => {
    if (!pasted.trim() || completing) return;
    setCompleting(true);
    try {
      await api().completeProviderOAuth(provider.id, state, pasted.trim());
      setMessage('Account connected');
      onDone();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Sign-in failed');
    } finally {
      setCompleting(false);
    }
  };

  const startDevice = async () => {
    setStarting(true);
    try {
      const res = await api().startProviderDeviceLogin(provider.id);
      setDevice({ pendingID: res.pending_id, userCode: res.user_code, url: res.verification_url });
      setDeviceStatus('waiting');
      window.open(res.verification_url, '_blank', 'noopener,noreferrer');
      const poll = async () => {
        try {
          const r = await api().pollProviderDeviceLogin(provider.id, res.pending_id);
          if (r.status === 'done') {
            setDeviceStatus('done');
            setMessage('Account connected');
            onDone();
            return;
          }
          if (r.status === 'error') {
            setDeviceStatus('error');
            setError(r.error || 'Sign-in failed');
            return;
          }
          pollTimer.current = window.setTimeout(() => void poll(), 5000);
        } catch (e) {
          setDeviceStatus('error');
          setError(e instanceof Error ? e.message : 'Sign-in polling failed');
        }
      };
      pollTimer.current = window.setTimeout(() => void poll(), 5000);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not start sign-in');
    } finally {
      setStarting(false);
    }
  };

  return (
    <div className="flex flex-col gap-4 px-5 py-4">
      <Text type="supporting">
        Sign in with your {vendorLabel(provider.vendor)} subscription. You can add several accounts — AgentRay spreads
        requests across them and skips one that hits its limit until it resets.
      </Text>

      {isCodex && !authURL ? (
        <div className="flex flex-col gap-3 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3">
          <Text type="supporting">Easiest: a one-time code on OpenAI's page — nothing to paste back.</Text>
          {device ? (
            <div className="flex items-center gap-3">
              <span className="font-mono text-lg tracking-widest text-[var(--color-text-primary)]">{device.userCode}</span>
              {deviceStatus === 'waiting' ? (
                <span className="flex items-center gap-1.5 text-xs text-[var(--color-text-secondary)]">
                  <Loader2 size={13} className="animate-spin" /> Waiting for you to authorize…
                </span>
              ) : null}
            </div>
          ) : (
            <Button variant="primary" size="sm" onClick={() => void startDevice()} disabled={starting}>
              {starting ? 'Starting…' : 'Sign in with a code'}
            </Button>
          )}
          {deviceStatus === 'error' ? (
            <Button variant="ghost" size="sm" onClick={() => void startDevice()}>
              Try again
            </Button>
          ) : null}
        </div>
      ) : null}

      {!device ? (
        <>
          {!authURL ? (
            <Button
              variant={isCodex ? 'ghost' : 'primary'}
              size="sm"
              icon={<ExternalLink size={14} />}
              onClick={() => void start()}
              disabled={starting}
            >
              {starting ? 'Starting…' : `Open ${vendorLabel(provider.vendor)} sign-in`}
            </Button>
          ) : (
            <div className="flex flex-col gap-3">
              <Text type="supporting">
                Finish signing in on the page that just opened. When the browser lands on a localhost address that
                can't load, copy the full URL from the address bar and paste it below.
              </Text>
              <a
                href={authURL}
                target="_blank"
                rel="noopener noreferrer"
                className="flex items-center gap-1 text-xs text-[var(--color-primary)] underline-offset-2 hover:underline"
              >
                <ExternalLink size={12} /> Re-open the sign-in page
              </a>
              <TextInput
                label="Redirect URL or code"
                value={pasted}
                placeholder="http://localhost:…/callback?code=…&state=…"
                onChange={(v) => setPasted(v)}
                width="100%"
              />
              <div className="flex gap-2">
                <Button variant="primary" size="sm" onClick={() => void complete()} disabled={!pasted.trim() || completing}>
                  {completing ? 'Connecting…' : 'Connect account'}
                </Button>
              </div>
            </div>
          )}
        </>
      ) : null}

      <div className="mt-1 flex gap-2 border-t border-[var(--color-border)] pt-3">
        <Button variant="ghost" size="sm" icon={<KeyRound size={14} />} onClick={onCancel}>
          Close
        </Button>
      </div>
    </div>
  );
}
