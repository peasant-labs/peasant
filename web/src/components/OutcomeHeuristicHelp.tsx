'use client';

import { CircleHelp } from 'lucide-react';
import { Button, Tooltip } from '@/lib/ft-ui';
import { cn } from '@/lib/utils';

/**
 * Shared help for every Peasant-owned surface that displays the session outcome.
 * The current payload carries only the computed label, so this explains the
 * ingest-time classifier without pretending an individual reason was supplied.
 */
export function OutcomeHeuristicHelp({ className }: { className?: string }) {
  return (
    <span className={cn('inline-flex', className)}>
      <Tooltip
        content={(
          <span className="block text-left">
            <span className="block">
              computed at ingest time from recorded error patterns, not a verified result.
            </span>
            <span className="mt-1 block">
              <strong>resolved:</strong> no notable recorded error pattern.
            </span>
            <span className="block">
              <strong>partial:</strong> some errors, an ending error, or a moderate error cluster.
            </span>
            <span className="block">
              <strong>failed:</strong> a high or concentrated error pattern.
            </span>
            <span className="mt-1 block">
              the current data includes the label, not a reason for an individual result.
            </span>
          </span>
        )}
      >
        <Button
          type="button"
          variant="ghost"
          size="sm"
          icon={CircleHelp}
          aria-label="how session outcomes are estimated"
        >
          outcome help
        </Button>
      </Tooltip>
    </span>
  );
}
