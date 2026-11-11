'use client';

import type { KickstartField, KickstartLine } from '@/lib/kickstart/model';
import { KickstartLineKind } from '@/lib/kickstart/values';

/**
 * Typed lines, as the server sends a preview or a privacy example: plain text,
 * a label, a before and after pair, or a spacer. The page draws them and adds
 * none of its own.
 */

const LINE_CLASS: ReadonlyMap<KickstartLineKind, string> = new Map<KickstartLineKind, string>([
  [KickstartLineKind.Text, 'ks-line'],
  [KickstartLineKind.Label, 'ks-line ks-line-label'],
  [KickstartLineKind.Before, 'ks-line ks-line-before'],
  [KickstartLineKind.After, 'ks-line ks-line-after'],
]);

export function ExampleLines({ lines, label }: { lines: readonly KickstartLine[]; label: string }) {
  return (
    <ul className="ks-lines" aria-label={label}>
      {lines.map((line, index) => {
        const key = `${index}:${line.kind.wire}`;
        if (line.kind === KickstartLineKind.Spacer) return <li key={key} className="ks-line-spacer" aria-hidden="true" />;
        const marker = line.kind === KickstartLineKind.Before || line.kind === KickstartLineKind.After ? line.kind.label : null;
        return (
          <li key={key} className={LINE_CLASS.get(line.kind) ?? 'ks-line'}>
            {marker !== null && <span className="sr-only">{marker}: </span>}
            {line.text}
          </li>
        );
      })}
    </ul>
  );
}

/**
 * The example a field carries, drawn beside its control. It is the server's
 * redaction of the current draft, so there is no sample here: when the server
 * withholds it and refuses the field instead, the refusal is shown and nothing
 * stands in for the example.
 */
export function FieldExample({ field }: { field: KickstartField }) {
  if (field.refusal !== null) {
    return (
      <p className="ks-refusal" role="alert">
        {field.refusal.error}
      </p>
    );
  }
  if (field.example.length === 0) return null;
  return (
    <div className="ks-example">
      <ExampleLines lines={field.example} label={`example for ${field.label}`} />
    </div>
  );
}
