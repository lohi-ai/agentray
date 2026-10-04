import { forwardRef, type HTMLAttributes } from 'react';
import { cn } from '../../lib/utils';

export type BadgeProps = HTMLAttributes<HTMLSpanElement> & {
  variant?: 'subtle' | 'solid' | 'outline';
  color?: 'default' | 'brand' | 'error' | 'success' | 'warning' | 'info';
  size?: 'default' | 'small';
};

export const Badge = forwardRef<HTMLSpanElement, BadgeProps>(function Badge({ className, variant = 'subtle', color = 'default', size = 'default', ...props }, ref) {
  return <span ref={ref} className={cn('lohi-badge', `lohi-badge--${variant}`, `lohi-badge--${color}`, `lohi-badge--${size}`, className)} {...props} />;
});
