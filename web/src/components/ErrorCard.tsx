import type { ReactNode } from 'react';

/**
 * The app's error card: a danger-tinted panel announced as an alert. One
 * treatment for every place that says something failed, so a failure reads the
 * same wherever it appears. Tokens only; square, as the design system draws
 * every surface.
 */
export const ERROR_CARD_CLASS = 'border border-danger/30 bg-danger-soft px-5 py-4';

export interface ErrorCardProps {
  readonly children: ReactNode;
  /** Layout the host adds around the card (margins, flex placement); never colour. */
  readonly className?: string;
}

export default function ErrorCard({ children, className }: ErrorCardProps) {
  return (
    <div className={className === undefined ? ERROR_CARD_CLASS : `${ERROR_CARD_CLASS} ${className}`} role="alert">
      {children}
    </div>
  );
}
