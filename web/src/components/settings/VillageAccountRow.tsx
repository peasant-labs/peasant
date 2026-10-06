'use client';

import { useState } from 'react';
import { CircleX, Loader } from 'lucide-react';
import type { LocalSetting, SyncAuthResponse } from '@peasant-labs/schema';
import { Button, Chip } from '@/lib/ft-ui';
import { logOutOfVillage } from '@/lib/api/settings';
import { tagOf } from '@/lib/settings/catalog';

function hostOf(url: string | undefined): string | undefined {
  if (!url) return undefined;
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/**
 * The Village sign-in on this computer, from GET /api/v1/sync/auth. `log out`
 * calls POST /api/v1/sync/logout, which removes the stored credential; the key
 * stays valid on Village until `peasant village logout` revokes it there.
 */
export function VillageAccountRow({ auth, readError, onRetry, connected, onLoggedOut }: {
  /** null while the sign-in is being read. */
  auth: SyncAuthResponse | null;
  readError?: string | null;
  onRetry?: () => void;
  /** The village.connected key, for its tag. */
  connected: LocalSetting | undefined;
  onLoggedOut: () => void | Promise<void>;
}) {
  const [status, setStatus] = useState<'idle' | 'pending' | 'failed'>('idle');
  const [error, setError] = useState<string | null>(null);
  const tag = connected ? tagOf(connected) : undefined;
  const signedIn = auth?.authenticated === true;
  const host = hostOf(auth?.villageUrl);

  const logOut = async () => {
    setStatus('pending');
    setError(null);
    try {
      await logOutOfVillage();
      setStatus('idle');
      await onLoggedOut();
    } catch (failure) {
      setStatus('failed');
      setError(failure instanceof Error ? failure.message : String(failure));
    }
  };

  const label = auth === null
    ? 'village sign-in'
    : signedIn
      ? <>connected as <strong className="stg-strong">@{auth.username}</strong></>
      : 'not connected';
  const help = auth === null
    ? readError ? 'the sign-in could not be read.' : 'reading the sign-in on this computer.'
    : signedIn
      ? `signed in with github${host ? ` on ${host}` : ''}. log out removes the key from this computer; peasant village logout also revokes it on village.`
      : 'sign in with peasant village login, or from the publish popup.';

  return (
    <div className={`srow srow-${status}`} data-status={status} data-setting-keys="village.connected">
      <span className="srow-text-col">
        <span className="srow-label-line">
          <span className="srow-label">{label}</span>
          {tag && <Chip size="sm" className="srow-tag">{tag}</Chip>}
        </span>
        <span className="srow-help">{help}</span>
        {readError && <span className="srow-error" role="alert">{readError}</span>}
        {status === 'failed' && <span className="srow-error" role="alert">{error}</span>}
      </span>
      {readError && onRetry && <Button variant="secondary" size="sm" onClick={onRetry}>retry sign-in</Button>}
      {signedIn && (
        <span className="srow-control">
          <Button variant="secondary" size="sm" onClick={logOut} disabled={status === 'pending'}>log out</Button>
          <span className={`srow-status srow-status-${status}`} aria-live="polite">
            {status === 'pending' && <><Loader className="srow-status-icon srow-spin" aria-hidden="true" /><span>logging out</span></>}
            {status === 'failed' && <><CircleX className="srow-status-icon" aria-hidden="true" /><span>not logged out</span></>}
          </span>
        </span>
      )}
    </div>
  );
}
