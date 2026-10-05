import { Loader2 } from 'lucide-react';
import type { ComponentProps } from 'react';
import { cn } from '../../lib/utils';

export function Spinner({ className, ...props }: ComponentProps<'svg'>) {
  return <Loader2 role="status" aria-label="Loading" className={cn('lohi-spinner', className)} {...props} />;
}
