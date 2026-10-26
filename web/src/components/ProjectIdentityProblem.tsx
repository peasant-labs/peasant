'use client';

import Link from 'next/link';
import { FolderX } from 'lucide-react';
import { EmptyState } from '@/lib/ft-ui';
import type { ProjectIdentityState } from '@/lib/navigation/useProjectIdentity';

type Problem = Extract<ProjectIdentityState, { phase: 'missing' | 'error' }>;

// What a session, map or changes page shows when its project cannot be opened:
// one plain line and a way forward. A project missing from this computer is a
// dead link, so home comes first; any other failure may pass, so trying again
// comes first and the server's own explanation sits behind "details", never
// the request that failed.
export function ProjectIdentityProblem({ state, onRetry }: { state: Problem; onRetry: () => void }) {
  const missing = state.phase === 'missing';
  const retry = (
    <button type="button" className={`btn btn-sm ${missing ? 'btn-secondary' : 'btn-primary'}`} onClick={onRetry}>
      try again
    </button>
  );
  const home = (
    <Link href="/" className={`btn btn-sm ${missing ? 'btn-primary' : 'btn-secondary'}`}>
      go to home
    </Link>
  );
  return (
    <div className="asb-page project-problem" role="alert" data-testid="project-identity-problem">
      <EmptyState
        as="h1"
        icon={FolderX}
        title={missing ? 'project not found' : 'couldn’t open this project'}
        message={missing
          ? 'peasant has no project at this link on this computer. the link may be old, or the project was removed.'
          : 'peasant couldn’t read this project just now.'}
        action={<div className="project-problem-actions">{missing ? <>{home}{retry}</> : <>{retry}{home}</>}</div>}
      />
      {!missing && state.message && (
        <details className="project-problem-details">
          <summary>details</summary>
          <p>{state.message}</p>
        </details>
      )}
    </div>
  );
}
