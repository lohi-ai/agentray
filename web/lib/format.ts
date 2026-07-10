// formatNumber renders a count with thousands separators (e.g. 24108 → 24,108).
export function formatNumber(value: number | undefined): string {
  if (value === undefined || value === null || Number.isNaN(value)) return '0';
  return new Intl.NumberFormat('en-US').format(value);
}

// formatCompact abbreviates large counts (1.84M, 12.4k) for stat strips.
export function formatCompact(value: number | undefined): string {
  if (!value) return '0';
  if (Math.abs(value) >= 1000) {
    return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(value);
  }
  return new Intl.NumberFormat('en-US').format(value);
}

// formatCost renders a USD amount with cents (e.g. 1.4 → $1.40).
export function formatCost(value: number | undefined): string {
  return `$${(value ?? 0).toFixed(2)}`;
}

// formatPercent renders a 0–100 number with a trailing % (e.g. 11.2 → 11.2%).
export function formatPercent(value: number | undefined, digits = 1): string {
  return `${(value ?? 0).toFixed(digits)}%`;
}

// formatDuration turns seconds into a compact human reading (2m 41s, 48s).
export function formatDuration(seconds: number | undefined): string {
  const total = Math.max(0, Math.round(seconds ?? 0));
  const m = Math.floor(total / 60);
  const s = total % 60;
  if (m <= 0) return `${s}s`;
  return `${m}m ${s.toString().padStart(2, '0')}s`;
}

// formatLatency renders milliseconds as seconds with one decimal (6400 → 6.4s).
export function formatLatency(ms: number | undefined): string {
  if (!ms) return '—';
  return `${(ms / 1000).toFixed(1)}s`;
}

// formatRelative renders how long ago a timestamp was, in the terse style the
// prototype uses (now, 18m, 2h, 3d). Empty/invalid input renders an em dash.
export function formatRelative(value: string | number | Date | undefined): string {
  if (!value) return '—';
  const then = new Date(value).getTime();
  if (Number.isNaN(then)) return '—';
  const diff = Date.now() - then;
  if (diff < 0) return 'now';
  const sec = Math.floor(diff / 1000);
  if (sec < 45) return 'now';
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h`;
  const day = Math.floor(hr / 24);
  if (day < 30) return `${day}d`;
  const mon = Math.floor(day / 30);
  if (mon < 12) return `${mon}mo`;
  return `${Math.floor(mon / 12)}y`;
}

// rangeLabel turns the active filter window (in hours) into the chip label the
// prototype uses (Last 24 hours, Last 7 days, …).
export function rangeLabel(hours: number | undefined): string {
  const h = hours ?? 24;
  if (h <= 1) return 'Last hour';
  if (h < 48) return `Last ${h} hours`;
  const days = Math.round(h / 24);
  if (days < 14) return `Last ${days} days`;
  if (days < 60) return `Last ${Math.round(days / 7)} weeks`;
  return `Last ${Math.round(days / 30)} months`;
}

export function formatDate(
  date: Date | string | number | undefined,
  opts: Intl.DateTimeFormatOptions = {},
) {
  if (!date) return "";

  try {
    return new Intl.DateTimeFormat("en-US", {
      month: opts.month ?? "long",
      day: opts.day ?? "numeric",
      year: opts.year ?? "numeric",
      ...opts,
    }).format(new Date(date));
  } catch (_err) {
    return "";
  }
}
