import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import { cn } from '../../lib/utils';
import { Spinner } from '../data-display/spinner';

export type ButtonProps = Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'color' | 'type'> & {
  type?: 'default' | 'text' | 'outlined';
  color?: 'brand' | 'neutral' | 'success' | 'warning' | 'error';
  size?: 'large' | 'middle' | 'small';
  shape?: 'default' | 'round' | 'square';
  block?: boolean;
  loading?: boolean;
  icon?: ReactNode;
  iconEnd?: ReactNode;
  htmlType?: 'button' | 'submit' | 'reset';
  href?: string;
  target?: string;
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button({
  className,
  type = 'default',
  color = 'brand',
  size = 'middle',
  shape = 'default',
  block = false,
  loading = false,
  icon,
  iconEnd,
  children,
  disabled,
  htmlType = 'button',
  href,
  target,
  ...props
}, ref) {
  const classes = cn('lohi-button', `lohi-button--${type}`, `lohi-button--${color}`, `lohi-button--${size}`, `lohi-button--${shape}`, block && 'lohi-button--block', className);
  const body = <>{loading ? <Spinner /> : icon}<span>{children}</span>{iconEnd}</>;
  if (href) {
    return <a className={classes} href={href} target={target} rel={target === '_blank' ? 'noopener noreferrer' : undefined} aria-disabled={disabled || loading}>{body}</a>;
  }
  return <button ref={ref} className={classes} disabled={disabled || loading} type={htmlType} {...props}>{body}</button>;
});
