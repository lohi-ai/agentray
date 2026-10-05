import { forwardRef, type HTMLAttributes, type ReactNode } from 'react';
import { cn } from '../../lib/utils';
import { Skeleton } from '../data-display/skeleton';

export type CardProps = Omit<HTMLAttributes<HTMLDivElement>, 'title'> & {
  size?: 'default' | 'small';
  bordered?: boolean;
  hoverable?: boolean;
  title?: ReactNode;
  extra?: ReactNode;
  loading?: boolean;
};

export const Card = forwardRef<HTMLDivElement, CardProps>(function Card({ className, size = 'default', bordered = true, hoverable = false, title, extra, loading = false, children, ...props }, ref) {
  return <div ref={ref} className={cn('lohi-card', `lohi-card--${size}`, bordered && 'lohi-card--bordered', hoverable && 'lohi-card--hoverable', className)} {...props}>{title ? <CardHeader extra={extra}><CardTitle>{title}</CardTitle></CardHeader> : null}{loading ? <div className="lohi-card__loading"><Skeleton /><Skeleton /><Skeleton /></div> : <CardContent>{children}</CardContent>}</div>;
});

export function CardHeader({ className, extra, children, ...props }: HTMLAttributes<HTMLDivElement> & { extra?: ReactNode }) {
  return <div className={cn('lohi-card__header', className)} {...props}><div>{children}</div>{extra}</div>;
}
export function CardTitle({ className, ...props }: HTMLAttributes<HTMLHeadingElement>) { return <h3 className={cn('lohi-card__title', className)} {...props} />; }
export function CardContent({ className, ...props }: HTMLAttributes<HTMLDivElement>) { return <div className={cn('lohi-card__content', className)} {...props} />; }
export function CardDescription({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) { return <p className={cn('lohi-card__description', className)} {...props} />; }
