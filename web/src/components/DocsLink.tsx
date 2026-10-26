import type { ReactNode } from 'react';
import { CircleHelp, ExternalLink } from 'lucide-react';
import { DOCS, type DocsTopic } from '@/lib/docs';

/**
 * A link to the docs page that teaches a task, opened in a new tab. The app
 * names what the reader can learn there and never restates the steps.
 *
 * - `button`: fairtrade's small secondary button, for an empty state's action.
 * - `inline`: a text link inside a sentence.
 * - `icon`: a square help button beside a heading, named by `children`.
 */
export function DocsLink({ topic, children, variant = 'button', className }: {
  topic: DocsTopic;
  children: ReactNode;
  variant?: 'button' | 'inline' | 'icon';
  className?: string;
}) {
  const href = DOCS[topic];
  const opens = <span className="sr-only">, opens the docs in a new tab</span>;
  if (variant === 'icon') {
    return (
      <a href={href} target="_blank" rel="noopener noreferrer" className={['btn btn-ghost btn-sm btn-icon docs-link-icon', className].filter(Boolean).join(' ')} title={typeof children === 'string' ? children : undefined} data-docs-topic={topic}>
        <CircleHelp aria-hidden="true" />
        <span className="sr-only">{children}</span>
        {opens}
      </a>
    );
  }
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" className={[variant === 'button' ? 'btn btn-secondary btn-sm docs-link' : 'docs-link-inline', className].filter(Boolean).join(' ')} data-docs-topic={topic}>
      {children}
      <ExternalLink aria-hidden="true" className="docs-link-glyph" />
      {opens}
    </a>
  );
}
