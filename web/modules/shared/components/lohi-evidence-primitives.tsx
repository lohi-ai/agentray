'use client';

import type { ReactNode } from 'react';
import { ArrowDown, ArrowUp } from 'lucide-react';
import { Alert, Badge, Button as LohiButton, Card, EmptyState as LohiEmptyState, Skeleton, Spinner } from '@/lib/lohi-ui';

type Tone = 'agent' | 'warning' | 'success' | 'danger';

export function Button({ children, variant, size, icon, onClick, disabled, isIconOnly, tooltip, className }: { children: ReactNode; variant: 'primary' | 'agent' | 'outline' | 'ghost'; size?: 'sm'; icon?: ReactNode; onClick?: () => void; disabled?: boolean; isIconOnly?: boolean; tooltip?: string; className?: string }) {
  const mapped = variant === 'primary' || variant === 'agent' ? { type: 'default' as const, color: 'brand' as const } : variant === 'outline' ? { type: 'outlined' as const, color: 'neutral' as const } : { type: 'text' as const, color: 'neutral' as const };
  return <LohiButton {...mapped} size="large" shape={isIconOnly ? 'square' : 'default'} aria-label={isIconOnly && typeof children === 'string' ? children : undefined} title={tooltip} disabled={disabled} onClick={onClick} className={className} icon={icon}>{children}</LohiButton>;
}

export function Panel({ title, action, children }: { title: string; action?: ReactNode; children: ReactNode }) {
  return <Card title={title} extra={action}>{children}</Card>;
}

const STATUS_COLOR: Record<string, 'default' | 'brand' | 'error' | 'success' | 'warning' | 'info'> = {
  working: 'info', healthy: 'success', ready: 'success', attention: 'warning', stale: 'warning', danger: 'error', error: 'error', paused: 'default', idle: 'default', immature: 'warning', denied: 'default',
};

export function StatusPill({ status, label, grow = true }: { status: string; label: string; grow?: boolean; pulse?: boolean }) {
  return <Badge color={STATUS_COLOR[status] ?? 'default'} className={grow ? 'ms-auto' : undefined}>{label}</Badge>;
}

export function Callout({ tone, icon, label, title, detail, action }: { tone: 'growth' | 'agentic' | 'warn'; icon: ReactNode; label: string; title: string; detail: string; action?: ReactNode }) {
  return <Alert role="status" variant={tone === 'growth' ? 'success' : tone === 'warn' ? 'warning' : 'brand'} customIcon={icon} title={<><span className="me-2 uppercase tracking-[0.06em]">{label}</span>{title}</>} description={detail} action={action} />;
}

export function EmptyState({ icon, title, detail, action }: { icon?: ReactNode; title: string; detail?: string; action?: ReactNode }) {
  return <LohiEmptyState icon={icon} title={title} description={detail} action={action} size="sm" />;
}

export function Loading({ label = 'Loading…' }: { label?: string }) {
  return <Card><div className="flex items-center gap-2" role="status"><Spinner /><span>{label}</span></div><Skeleton /></Card>;
}

export function StatsStrip({ stats }: { stats: Array<{ label: string; value: string; tone?: Tone; delta?: string; deltaTone?: 'up' | 'down'; badge?: { status: string; label: string }; provenance?: string }> }) {
  return <Card><div className="grid grid-cols-[repeat(auto-fit,minmax(140px,1fr))]">{stats.map((stat) => <div key={stat.label} className="flex flex-col gap-1 px-4 py-3"><span className="text-xs text-[var(--lohi-muted-foreground)]">{stat.label}</span><strong className="text-xl tabular-nums">{stat.value}</strong>{stat.badge ? <StatusPill status={stat.badge.status} label={stat.badge.label} grow={false} /> : null}{stat.delta ? <span className={stat.deltaTone === 'down' ? 'text-[var(--lohi-error-700)]' : 'text-[var(--lohi-success-700)]'}>{stat.deltaTone === 'down' ? <ArrowDown size={13} /> : <ArrowUp size={13} />}{stat.delta}</span> : null}{stat.provenance ? <span className="text-xs text-[var(--lohi-muted-foreground)]">{stat.provenance}</span> : null}</div>)}</div></Card>;
}
