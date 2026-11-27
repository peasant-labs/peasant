'use client';

import type { KickstartConnect } from '@/lib/kickstart/model';
import type { VillageSignIn } from '@/lib/kickstart/useVillageSignIn';
import { SignInState } from './ConnectionStep';

/** The id of the offer's prompt, which names the offer's answers in the foot. */
export const CONNECT_OFFER_PROMPT_ID = 'ks-connect-offer';

/**
 * The village connect offer at a boundary between stops. The server decided
 * that it is due (`connect.offered`) and wrote its words; this draws the
 * prompt and how the browser sign-in is going. Its answers are drawn in the
 * frame's foot (`FootAnswers`), named by the prompt. Saying yes opens the
 * existing browser sign-in, and the answer is recorded only once this computer
 * is signed in, so a sign-in that fails leaves the offer where it was.
 */
export function ConnectOffer({ connect, signIn }: { connect: KickstartConnect; signIn: VillageSignIn }) {
  return (
    <section className="ks-field" aria-labelledby={CONNECT_OFFER_PROMPT_ID}>
      <h2 className="ks-title" id={CONNECT_OFFER_PROMPT_ID}>
        {connect.prompt ?? 'connect to peasant online now?'}
      </h2>
      <p className="ks-help">connecting signs this computer in. it publishes nothing.</p>
      <SignInState signIn={signIn} />
    </section>
  );
}
