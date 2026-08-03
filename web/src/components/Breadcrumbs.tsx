'use client';

import Link from 'next/link';
import { ChevronRightIcon } from 'lucide-react';
import { cn } from '@/lib/utils';

interface BreadcrumbItem {
  label: string;
  href?: string;
}

interface BreadcrumbsProps {
  items: BreadcrumbItem[];
  className?: string;
}

export function Breadcrumbs({ items, className }: BreadcrumbsProps) {
  return (
    <nav aria-label="Breadcrumb" className={cn('flex items-center gap-1 text-xs', className)}>
      {items.map((item, i) => {
        const isLast = i === items.length - 1;
        return (
          <span key={i} className="flex items-center gap-1">
            {i > 0 && <ChevronRightIcon className="size-3 text-ink-4 shrink-0" />}
            {isLast || !item.href ? (
              <span className={cn(isLast ? 'text-ink font-medium truncate max-w-[200px]' : 'text-ink-3')} title={item.label}>
                {item.label}
              </span>
            ) : (
              <Link
                href={item.href}
                className="text-ink-3 hover:text-ink transition-colors focus-mono"
              >
                {item.label}
              </Link>
            )}
          </span>
        );
      })}
    </nav>
  );
}
