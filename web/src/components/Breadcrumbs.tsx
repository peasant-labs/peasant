'use client';

import { Fragment } from 'react';
import Link from 'next/link';
import { ChevronRight } from 'lucide-react';
import { cn } from '@/lib/utils';

interface BreadcrumbItem {
  label: string;
  href?: string;
}

interface BreadcrumbsProps {
  items: BreadcrumbItem[];
  className?: string;
}

/**
 * Breadcrumbs, rendered with fairtrade's own `.crumb` classes.
 *
 * The transcript viewer draws its trail inside the design system's hero, using
 * `.crumb` / `.link` / `.cur`. This component used to draw a hand-rolled trail
 * with Tailwind utilities instead — same information, visibly different chrome
 * (sans vs mono, no dotted link underline, different size and casing), so the
 * same trail changed appearance as you moved between pages.
 *
 * Emitting the DS classes means both surfaces are styled by one stylesheet and
 * stay in step through any future design-system change, rather than being two
 * implementations someone has to remember to update together.
 */
export function Breadcrumbs({ items, className }: BreadcrumbsProps) {
  return (
    <nav aria-label="Breadcrumb" className={cn('crumb', className)}>
      {items.map((item, i) => {
        const isLast = i === items.length - 1;
        return (
          <Fragment key={`${item.label}-${i}`}>
            {i > 0 && <ChevronRight size={13} aria-hidden />}
            {!isLast && item.href ? (
              <Link className="link" href={item.href} title={item.label}>
                {item.label}
              </Link>
            ) : (
              <span className={isLast ? 'cur' : 'link'} title={item.label}>
                {item.label}
              </span>
            )}
          </Fragment>
        );
      })}
    </nav>
  );
}
