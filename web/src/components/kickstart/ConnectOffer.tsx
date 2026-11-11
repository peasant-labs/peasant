'use client';

import { Button } from '@/lib/ft-ui';
import type { KickstartConnect } from '@/lib/kickstart/model';
import type { VillageSignIn } from '@/lib/kickstart/useVillageSignIn';
import { KickstartConnectAnswer } from '@/lib/kickstart/values';

/**
 * The village connect offer at a boundary between stops. The server decided
 * that it is due (`connect.offered`) and wrote its words; this draws it and
 * records the answer. Saying yes opens the existing browser sign-in, and the
 * answer is recorded only once this computer is signed in, so a sign-in that
 * fails leaves the offer where it was.
 */
export function ConnectOffer({
  connect,
  signIn,
  answer,
}: {
  connect: KickstartConnect;
  signIn: VillageSignIn;
  answer: (answer: KickstartConnectAnswer) => Promise<boolean>;
}) {
  const accept = async () => {
    if (await signIn.signIn()) await answer(KickstartConnectAnswer.Accept);
  };
  return (
    <section className="ks-field" aria-labelledby="ks-connect-offer">
      <h2 className="ks-title" id="ks-connect-offer">
        {connect.prompt ?? 'connect to peasant online now?'}
      </h2>
      <p className="ks-help">connecting signs this computer in. it publishes nothing.</p>
      <div className="ks-answers">
        <Button variant="primary" loading={signIn.waiting} disabled={signIn.waiting} onClick={() => void accept()}>
          {KickstartConnectAnswer.Accept.label}
        </Button>
        <Button variant="secondary" disabled={signIn.waiting} onClick={() => void answer(KickstartConnectAnswer.Skip)}>
          {connect.skip ?? KickstartConnectAnswer.Skip.label}
        </Button>
        {signIn.waiting && (
          <Button variant="ghost" onClick={signIn.cancel}>
            stop waiting
          </Button>
        )}
      </div>
      {signIn.waiting && (
        <p className="ks-help" role="status">
          finish signing in on the page that opened in your browser.
        </p>
      )}
      {signIn.failure !== null && (
        <p className="ks-refusal" role="alert">
          {signIn.failure}
        </p>
      )}
    </section>
  );
}
