'use client';

import { Check, Info, TriangleAlert, X } from 'lucide-react';
import { useState, type HTMLAttributes, type ReactNode } from 'react';
import { cn } from '../../lib/utils';

export type AlertProps = Omit<HTMLAttributes<HTMLDivElement>, 'title'> & {
  title?: ReactNode;
  description?: ReactNode;
  size?: 'small' | 'default' | 'large';
  variant?: 'default' | 'brand' | 'info' | 'success' | 'warning' | 'error';
  closable?: boolean;
  onClose?: () => void;
  icon?: boolean;
  customIcon?: ReactNode;
  action?: ReactNode;
  role?: 'alert' | 'status';
};

export function Alert({ title, description, size = 'default', variant = 'default', closable = false, onClose, children, className, icon = true, customIcon, action, role = 'alert', ...props }: AlertProps) {
  const [visible, setVisible] = useState(true);
  if (!visible) return null;
  const Glyph = variant === 'success' ? Check : variant === 'warning' || variant === 'error' ? TriangleAlert : Info;
  return (
    <div className={cn('lohi-alert', `lohi-alert--${size}`, `lohi-alert--${variant}`, className)} role={role} {...props}>
      {icon ? <span className="lohi-alert__icon" aria-hidden>{customIcon ?? <Glyph size={16} />}</span> : null}
      <div className="lohi-alert__content">{title ? <div className="lohi-alert__title">{title}</div> : null}{description ? <div>{description}</div> : null}{children}</div>
      {action ? <div className="lohi-alert__action">{action}</div> : null}
      {closable ? <button type="button" className="lohi-alert__close" aria-label="Close" onClick={() => { setVisible(false); onClose?.(); }}><X size={16} /></button> : null}
    </div>
  );
}
