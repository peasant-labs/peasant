'use client';

import { Button } from '@/lib/ft-ui';
import type { KickstartField } from '@/lib/kickstart/model';
import type { VillageSignIn } from '@/lib/kickstart/useVillageSignIn';
import { KickstartConnectAnswer } from '@/lib/kickstart/values';

/**
 * The village stop at the start of the wizard. Its action field offers the
 * server's two answers. Saying yes opens the existing browser sign-in first
 * and sends the answer once this computer is signed in; saying no sends it at
 * once. The village question is asked again at the license boundary if the
 * server still says it is due; this step never decides that.
 */
export function ConnectionStep({
  field,
  signIn,
  answer,
}: {
  field: KickstartField;
  signIn: VillageSignIn;
  answer: (key: string, value: string) => Promise<void>;
}) {
  const choose = async (token: string) => {
    if (token === KickstartConnectAnswer.Accept.wire && !(await signIn.signIn())) return;
    await answer(field.key, token);
  };
  return (
    <div className="ks-field">
      <span className="ks-field-label" id={`${field.key}-label`}>
        {field.label}
      </span>
      {field.description !== null && <p className="ks-help">{field.description}</p>}
      <div className="ks-answers" role="group" aria-labelledby={`${field.key}-label`}>
        {field.options.map((option) => (
          <Button
            key={option.value}
            variant={option.value === KickstartConnectAnswer.Accept.wire ? 'primary' : 'secondary'}
            loading={signIn.waiting && option.value === KickstartConnectAnswer.Accept.wire}
            disabled={signIn.waiting}
            onClick={() => void choose(option.value)}
          >
            {option.label}
          </Button>
        ))}
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
    </div>
  );
}
