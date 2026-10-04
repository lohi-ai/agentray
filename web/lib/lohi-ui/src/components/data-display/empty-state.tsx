import { forwardRef, type HTMLAttributes, type ReactNode } from 'react';
import { cn } from '../../lib/utils';

export type EmptyStateProps = HTMLAttributes<HTMLDivElement> & {
  icon?: ReactNode;
  title: string;
  description?: string;
  action?: ReactNode;
  size?: 'default' | 'sm' | 'lg';
  titleAs?: 'h1' | 'h2' | 'h3';
};

export const EmptyState = forwardRef<HTMLDivElement, EmptyStateProps>(function EmptyState({ className, icon, title, description, action, size = 'default', titleAs: Title = 'h3', ...props }, ref) {
  return <div ref={ref} className={cn('lohi-empty-state', `lohi-empty-state--${size}`, className)} role="status" {...props}>{icon ? <div className="lohi-empty-state__icon">{icon}</div> : null}<div><Title>{title}</Title>{description ? <p>{description}</p> : null}</div>{action}</div>;
});
