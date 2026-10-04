'use client';

import * as React from 'react';
import * as AccordionPrimitive from '../../vendor/radix-accordion.mjs';
import { ChevronDownIcon } from 'lucide-react';
import { cn } from '../../lib/utils';

const Accordion = AccordionPrimitive.Root;

const AccordionItem = React.forwardRef<
  React.ElementRef<typeof AccordionPrimitive.Item>,
  React.ComponentPropsWithoutRef<typeof AccordionPrimitive.Item>
>(({ className, ...props }, ref) => (
  <AccordionPrimitive.Item
    ref={ref}
    data-slot='accordion-item'
    className={cn(
      'border-b border-neutral-200 last:border-b-0 dark:border-neutral-800',
      className
    )}
    {...props}
  />
));
AccordionItem.displayName = 'AccordionItem';

const AccordionTrigger = React.forwardRef<
  React.ElementRef<typeof AccordionPrimitive.Trigger>,
  React.ComponentPropsWithoutRef<typeof AccordionPrimitive.Trigger>
>(({ className, children, ...props }, ref) => (
  <AccordionPrimitive.Header className='flex'>
    <AccordionPrimitive.Trigger
      ref={ref}
      data-slot='accordion-trigger'
      className={cn(
        'flex flex-1 items-start justify-between gap-4 rounded-md py-4 text-left text-sm font-medium transition-all outline-none',
        'text-neutral-900 hover:underline dark:text-neutral-100',
        'focus-visible:ring-[3px] focus-visible:ring-brand-500/40 focus-visible:border-brand-500',
        'disabled:pointer-events-none disabled:opacity-50',
        '[&[data-state=open]>svg]:rotate-180',
        className
      )}
      {...props}
    >
      {children}
      <ChevronDownIcon className='text-neutral-500 dark:text-neutral-400 pointer-events-none size-4 shrink-0 translate-y-0.5 transition-transform duration-200' />
    </AccordionPrimitive.Trigger>
  </AccordionPrimitive.Header>
));
AccordionTrigger.displayName = 'AccordionTrigger';

const AccordionContent = React.forwardRef<
  React.ElementRef<typeof AccordionPrimitive.Content>,
  React.ComponentPropsWithoutRef<typeof AccordionPrimitive.Content>
>(({ className, children, ...props }, ref) => (
  <AccordionPrimitive.Content
    ref={ref}
    data-slot='accordion-content'
    className='data-[state=closed]:animate-accordion-up data-[state=open]:animate-accordion-down overflow-hidden text-sm text-neutral-700 dark:text-neutral-300'
    {...props}
  >
    <div className={cn('pt-0 pb-4', className)}>{children}</div>
  </AccordionPrimitive.Content>
));
AccordionContent.displayName = 'AccordionContent';

export { Accordion, AccordionItem, AccordionTrigger, AccordionContent };
