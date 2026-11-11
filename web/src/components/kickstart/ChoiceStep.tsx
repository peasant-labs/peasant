'use client';

import { Button, Checkbox } from '@/lib/ft-ui';
import type { KickstartField, LocalSettingValue } from '@/lib/kickstart/model';
import { isOptionSelected } from '@/lib/kickstart/view';
import { KickstartFieldKind } from '@/lib/kickstart/values';

/**
 * The control for one served field. The field kind picks the control; the
 * server decides everything else: whether the field exists, its options, its
 * label, its value and whether that value is acceptable. A refusal the server
 * attached to the field is shown with it.
 */

export type AnswerField = (key: string, value: LocalSettingValue) => void;

function Refusal({ field, hidden }: { field: KickstartField; hidden: boolean }) {
  if (hidden || field.refusal === null) return null;
  return (
    <p className="ks-refusal" role="alert">
      {field.refusal.error}
    </p>
  );
}

function ChoiceGroup({ field, answer, hideRefusal }: { field: KickstartField; answer: AnswerField; hideRefusal: boolean }) {
  return (
    <fieldset className="ks-field">
      <legend>{field.label}</legend>
      {field.description !== null && <p className="ks-help">{field.description}</p>}
      <div className="ks-options" role="radiogroup" aria-label={field.label}>
        {field.options.map((option) => {
          const describedBy = option.description === null ? undefined : `${field.key}-${option.value}-desc`;
          return (
            <div key={option.value} className="ks-option">
              <label className="is-radio">
                <input
                  type="radio"
                  className="is-radio-dot"
                  name={field.key}
                  value={option.value}
                  checked={isOptionSelected(field, option)}
                  aria-describedby={describedBy}
                  onChange={() => answer(field.key, option.value)}
                />
                <span>{option.label}</span>
              </label>
              {option.description !== null && (
                <p className="ks-option-desc" id={describedBy}>
                  {option.description}
                </p>
              )}
            </div>
          );
        })}
      </div>
      <Refusal field={field} hidden={hideRefusal} />
    </fieldset>
  );
}

function ToggleControl({ field, answer, hideRefusal }: { field: KickstartField; answer: AnswerField; hideRefusal: boolean }) {
  return (
    <div className="ks-field">
      <Checkbox checked={field.value === true} onChange={(checked) => answer(field.key, checked)}>
        {field.label}
      </Checkbox>
      {field.description !== null && <p className="ks-help">{field.description}</p>}
      <Refusal field={field} hidden={hideRefusal} />
    </div>
  );
}

/** An action field is a question with labelled answers; each answer is a button. */
function ActionControl({ field, answer, hideRefusal }: { field: KickstartField; answer: AnswerField; hideRefusal: boolean }) {
  return (
    <div className="ks-field">
      <span className="ks-field-label" id={`${field.key}-label`}>
        {field.label}
      </span>
      {field.description !== null && <p className="ks-help">{field.description}</p>}
      <div className="ks-actions" role="group" aria-labelledby={`${field.key}-label`}>
        {field.options.map((option) => (
          <Button key={option.value} variant="secondary" onClick={() => answer(field.key, option.value)}>
            {option.label}
          </Button>
        ))}
      </div>
      <Refusal field={field} hidden={hideRefusal} />
    </div>
  );
}

export function NoticeText({ field }: { field: KickstartField }) {
  return <p className="ks-intro">{field.description ?? field.label}</p>;
}

/**
 * One field of a config stop. The tree field is drawn by the selection step,
 * which owns the forest, the preview and the scan; it returns nothing here.
 * `hideRefusal` leaves the server's refusal to a neighbour that draws it, such
 * as the example pane beside a privacy choice.
 */
export function FieldControl({ field, answer, hideRefusal = false }: { field: KickstartField; answer: AnswerField; hideRefusal?: boolean }) {
  if (field.kind === KickstartFieldKind.Toggle) return <ToggleControl field={field} answer={answer} hideRefusal={hideRefusal} />;
  if (field.kind === KickstartFieldKind.Choice) return <ChoiceGroup field={field} answer={answer} hideRefusal={hideRefusal} />;
  if (field.kind === KickstartFieldKind.Action) return <ActionControl field={field} answer={answer} hideRefusal={hideRefusal} />;
  if (field.kind === KickstartFieldKind.Notice) return <NoticeText field={field} />;
  return null;
}
