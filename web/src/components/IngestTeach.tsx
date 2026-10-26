import { HardDrive } from 'lucide-react';
import { EmptyState } from '@/lib/ft-ui';
import { DocsLink } from '@/components/DocsLink';

/**
 * The ONE "nothing recorded yet" empty state, shared by every surface that can
 * be empty because nothing has been recorded: the Home project view, the Map
 * picker, a project's session list. How to record sessions lives in the docs,
 * so the state names what is missing and links there.
 */
export function IngestTeach({ headline = 'no ai work recorded yet' }: { headline?: string }) {
  return (
    <EmptyState
      icon={HardDrive}
      title={headline}
      action={<DocsLink topic="record">how to record sessions</DocsLink>}
    />
  );
}
