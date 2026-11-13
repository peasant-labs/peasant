'use client';

import type { KickstartField } from '@/lib/kickstart/model';
import type { VillageSignIn } from '@/lib/kickstart/useVillageSignIn';
import { answerGroupLabelId } from '@/lib/kickstart/view';

/**
 * The village stop at the start of the wizard: its question, its help, and
 * how the browser sign-in is going. The server's two answers are drawn in the
 * frame's foot (`FootAnswers`), where `continue` sits on the other stops, and
 * the question here names them. Saying yes opens the existing browser sign-in
 * first and sends the answer once this computer is signed in; saying no sends
 * it at once. The village question is asked again at the license boundary if
 * the server still says it is due; this step never decides that.
 */
export function ConnectionStep({ field, signIn }: { field: KickstartField; signIn: VillageSignIn }) {
  return (
    <div className="ks-field">
      <span className="ks-field-label" id={answerGroupLabelId(field)}>
        {field.label}
      </span>
      {field.description !== null && <p className="ks-help">{field.description}</p>}
      <SignInState signIn={signIn} />
    </div>
  );
}

/** What the browser sign-in is doing: waiting for the reader, or why it failed. */
export function SignInState({ signIn }: { signIn: VillageSignIn }) {
  return (
    <>
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
    </>
  );
}
