import { clsx, type ClassValue } from '../vendor/clsx.mjs';
import { twMerge } from '../vendor/tailwind-merge.mjs';

/**
 * Combines multiple class names and Tailwind CSS classes,
 * correctly handling conflicts through tailwind-merge.
 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
