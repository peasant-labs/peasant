'use client';

import { useEffect, useState } from 'react';
import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import { zSessionsPayload } from '@peasant-labs/schema';
import { FeedbackPanel } from '@/lib/ft-ui';
import { getApiBaseUrl } from '@/lib/api/base';
import { parseProjectHash, transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { ShareWizardClient } from './ShareWizardClient';

/**
 * `/share` is the canonical publish route. A link that names one session and
 * no wizard step (`/share?sessionId=<id>`) opens that transcript with its
 * publish popup, the one place a single transcript is published from. Every
 * other link, and a plain visit, opens the multi-session wizard, which stays a
 * deprecation candidate.
 */
export function SharePageClient() {
  const searchParams = useSearchParams();
  const sessionId = searchParams?.get('sessionId')?.trim() || null;
  const wizardLink = searchParams?.has('step') || searchParams?.has('sessions');
  if (sessionId && !wizardLink) return <OpenTranscriptPublish sessionId={sessionId} />;
  return <ShareWizardClient />;
}

/**
 * Resolve the session's project and replace this page with its transcript,
 * opened on the publish popup. The lookup applies no list scope: a session the
 * saved lists leave out still opens by its link.
 */
async function resolveProjectHash(sessionId: string): Promise<ProjectHash> {
  const path = `/api/v1/session-summaries?ids=${encodeURIComponent(sessionId)}`;
  const response = await fetch(`${getApiBaseUrl()}${path}`);
  const body = await response.json().catch(() => null) as unknown;
  if (!response.ok) {
    const message = body && typeof body === 'object' && typeof (body as { error?: unknown }).error === 'string'
      ? (body as { error: string }).error
      : `the server answered HTTP ${response.status}`;
    throw new Error(`Session ${sessionId} could not be opened for publishing: ${message}`);
  }
  const parsed = zSessionsPayload.safeParse(body);
  const summary = parsed.success ? parsed.data.sessions?.find((session) => session.id === sessionId) : undefined;
  const projectHash = parseProjectHash(summary?.projectHash);
  if (!projectHash) {
    throw new Error(`Session ${sessionId} is not recorded on this computer, so there is no transcript to publish. Open the session from its project, or record it with /peasant, then publish it from its page.`);
  }
  return projectHash;
}

function OpenTranscriptPublish({ sessionId }: { sessionId: string }) {
  const router = useRouter();
  const [failure, setFailure] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    resolveProjectHash(sessionId)
      .then((projectHash) => {
        if (live) router.replace(transcriptHref(projectHash, sessionId, { publish: true }));
      })
      .catch((error: unknown) => {
        if (live) setFailure(error instanceof Error ? error.message : String(error));
      });
    return () => {
      live = false;
    };
  }, [router, sessionId]);

  if (failure) {
    return (
      <div className="max-w-[1600px] mx-auto px-6 pt-6" role="alert">
        <FeedbackPanel variant="error" title="the transcript could not be opened">
          {failure}{' '}
          <Link href="/share?step=select" className="link">publish several sessions instead</Link>
        </FeedbackPanel>
      </div>
    );
  }
  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6" role="status">
      <p className="font-mono text-[14px] text-ink-3">opening the transcript to publish it</p>
    </div>
  );
}
