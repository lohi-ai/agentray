'use client';

import { ChevronDown } from 'lucide-react';
import { createContext, forwardRef, useContext, useId, useMemo, useState, type ButtonHTMLAttributes, type HTMLAttributes, type ReactNode } from 'react';
import { cn } from '../../lib/utils';

type RootValue = { open: Set<string>; toggle: (value: string) => void };
const RootContext = createContext<RootValue | null>(null);
const ItemContext = createContext<{ value: string; open: boolean; triggerId: string; panelId: string } | null>(null);

export function Accordion({ children, defaultValue, className }: { children: ReactNode; defaultValue?: string | string[]; className?: string; type?: 'single' | 'multiple'; collapsible?: boolean }) {
  const [open, setOpen] = useState(() => new Set(Array.isArray(defaultValue) ? defaultValue : defaultValue ? [defaultValue] : []));
  const context = useMemo<RootValue>(() => ({ open, toggle: (value) => setOpen((current) => { const next = new Set(current); if (next.has(value)) next.delete(value); else next.add(value); return next; }) }), [open]);
  return <RootContext.Provider value={context}><div className={cn('lohi-accordion', className)}>{children}</div></RootContext.Provider>;
}

export const AccordionItem = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement> & { value: string }>(function AccordionItem({ value, className, children, ...props }, ref) {
  const root = useContext(RootContext);
  const uid = useId();
  if (!root) throw new Error('AccordionItem must be inside Accordion');
  const item = { value, open: root.open.has(value), triggerId: `lohi-accordion-trigger-${uid}`, panelId: `lohi-accordion-panel-${uid}` };
  return <ItemContext.Provider value={item}><div ref={ref} className={cn('lohi-accordion__item', className)} {...props}>{children}</div></ItemContext.Provider>;
});

export const AccordionTrigger = forwardRef<HTMLButtonElement, ButtonHTMLAttributes<HTMLButtonElement>>(function AccordionTrigger({ className, children, ...props }, ref) {
  const root = useContext(RootContext); const item = useContext(ItemContext);
  if (!root || !item) throw new Error('AccordionTrigger must be inside AccordionItem');
  return <h3 className="lohi-accordion__heading"><button ref={ref} type="button" id={item.triggerId} aria-expanded={item.open} aria-controls={item.panelId} className={cn('lohi-accordion__trigger', className)} onClick={() => root.toggle(item.value)} {...props}>{children}<ChevronDown className="lohi-accordion__chevron" size={16} aria-hidden /></button></h3>;
});

export const AccordionContent = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement>>(function AccordionContent({ className, ...props }, ref) {
  const item = useContext(ItemContext);
  if (!item) throw new Error('AccordionContent must be inside AccordionItem');
  if (!item.open) return null;
  return <div ref={ref} id={item.panelId} role="region" aria-labelledby={item.triggerId} className={cn('lohi-accordion__content', className)} {...props} />;
});
