'use client';

import type { KickstartSession, KickstartStop } from '@/lib/kickstart/model';
import type { KickstartSessionApi } from '@/lib/kickstart/useKickstartSession';
import type { VillageSignIn } from '@/lib/kickstart/useVillageSignIn';
import { KickstartFieldKind, KickstartStopKind } from '@/lib/kickstart/values';
import { FieldControl, NoticeText } from './ChoiceStep';
import { ConnectionStep } from './ConnectionStep';
import { FieldExample } from './PrivacyStep';
import { ReviewStep } from './ReviewStep';
import { SelectionStep } from './SelectionStep';

/**
 * One served stop. The stop kind picks the layout (a connection question, a
 * configuration section, the review) and each field's kind picks its control.
 * Nothing else is decided here: the stops, their order, which are visible, the
 * words, the options and the values are the server's, drawn as served.
 */
export function StopRenderer({
  stop,
  session,
  api,
  signIn,
  answeredConnection,
}: {
  stop: KickstartStop;
  session: KickstartSession;
  api: KickstartSessionApi;
  signIn: VillageSignIn;
  /** Called once the connection question's answer has been recorded. */
  answeredConnection: () => void;
}) {
  const answer = (key: string, value: Parameters<KickstartSessionApi['answer']>[1]) => void api.answer(key, value);
  const wide =
    stop.kind === KickstartStopKind.Review ||
    stop.fields.some((field) => field.kind === KickstartFieldKind.Tree || field.example.length > 0);

  return (
    <div className={wide ? 'ks-stop ks-stop-wide' : 'ks-stop'}>
      <h2 className="ks-title">{stop.title}</h2>
      {stop.intro !== null && <p className="ks-intro">{stop.intro}</p>}
      {stop.hints.length > 0 && (
        <ul className="ks-hints">
          {stop.hints.map((hint) => (
            <li key={hint} className="ks-hint">
              {hint}
            </li>
          ))}
        </ul>
      )}

      {stop.kind === KickstartStopKind.Review ? (
        <ReviewStep stop={stop} session={session} api={api} />
      ) : (
        stop.fields.map((field) => {
          if (field.kind === KickstartFieldKind.Tree) return <SelectionStep key={field.key} field={field} api={api} />;
          if (stop.kind === KickstartStopKind.Connection && field.kind === KickstartFieldKind.Action) {
            return (
              <ConnectionStep
                key={field.key}
                field={field}
                signIn={signIn}
                answer={async (key, value) => {
                  if (await api.answer(key, value)) answeredConnection();
                }}
              />
            );
          }
          if (field.kind === KickstartFieldKind.Notice) return <NoticeText key={field.key} field={field} />;
          if (field.example.length > 0) {
            return (
              <div key={field.key} className="ks-split">
                <FieldControl field={field} answer={answer} hideRefusal />
                <FieldExample field={field} />
              </div>
            );
          }
          return <FieldControl key={field.key} field={field} answer={answer} />;
        })
      )}
    </div>
  );
}
