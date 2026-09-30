'use client';

import { useState, type ReactNode } from 'react';
import { LocalSettingKind, type LocalSetting } from '@peasant-labs/schema';
import { Chip, SettingRow } from '@/lib/ft-ui';
import { updateSetting } from '@/lib/api/settings';
import { copyOf, describeValue, optionLabel, overridden, shownValue, tagOf } from '@/lib/settings/catalog';

/** Called with the row the server saved, so the page shows what applies now. */
export type OnSettingSaved = (saved: LocalSetting) => void;

interface SettingKeyRowProps {
  setting: LocalSetting;
  onSaved: OnSettingSaved;
  /** Replaces the catalog label, for a row that leads with a provider mark. */
  label?: ReactNode;
  /** Replaces the catalog help. */
  help?: ReactNode;
  /** Every key the row shows, when it shows more than its own. */
  keys?: readonly string[];
  /** Leaves the tag off a row whose neighbour carries it for both. */
  hideTag?: boolean;
}

/** The text a text control shows for a value. */
function textOf(setting: LocalSetting, value: unknown): string {
  if (value === null || value === undefined) return '';
  if (setting.kind === LocalSettingKind.StringList && Array.isArray(value)) return value.join(', ');
  return String(value);
}

/**
 * The JSON a control's next value sends. Empty text unsets the key, so its
 * default applies again. A whole number is required for an integer key; other
 * input is refused here, before any request, and the reason shows on the row.
 */
function wireValue(setting: LocalSetting, next: boolean | string): unknown {
  switch (setting.kind) {
    case LocalSettingKind.Boolean:
      return Boolean(next);
    case LocalSettingKind.Choice:
      return String(next);
    case LocalSettingKind.Integer: {
      const text = String(next).trim();
      if (text === '') return null;
      if (!/^-?\d+$/.test(text)) throw new Error(`"${text}" is not a whole number. enter one, or leave it empty for the default.`);
      return Number(text);
    }
    case LocalSettingKind.StringList: {
      const items = String(next).split(',').map((item) => item.trim()).filter(Boolean);
      return items.length === 0 ? null : items;
    }
    default: {
      const text = String(next).trim();
      return text === '' ? null : text;
    }
  }
}

/** A key the page may not change: its value, and how it changes instead. */
export function ReadOnlyRow({ label, value, help, tag, keys, action }: {
  label: ReactNode;
  value: ReactNode;
  help?: ReactNode;
  tag?: string;
  keys: readonly string[];
  action?: ReactNode;
}) {
  return (
    <div className="srow stg-readonly" data-setting-keys={keys.join(' ')}>
      <span className="srow-text-col">
        <span className="srow-label-line">
          <span className="srow-label">{label}</span>
          {tag && <Chip size="sm" className="srow-tag">{tag}</Chip>}
        </span>
        {help && <span className="srow-help">{help}</span>}
      </span>
      <span className="srow-control stg-readonly-control">
        <span className="stg-readonly-value">{value}</span>
        {action}
        {/* the status slot a saving row keeps, so values line up with the controls above and below */}
        <span className="srow-status" aria-hidden="true" />
      </span>
    </div>
  );
}

/**
 * One setting with the control its kind takes: a switch, a menu, or a text
 * field that saves on `save`. Each change is one PATCH. A refused change puts
 * the previous value back and shows the server's reason.
 */
export function SettingKeyRow({ setting, onSaved, label, help, keys, hideTag = false }: SettingKeyRowProps) {
  // Remounts the row when a save lands on the value it already showed (a
  // cleared field whose default applies again), so the field shows it.
  const [resync, setResync] = useState(0);
  const copy = copyOf(setting.key);
  const rowKeys = (keys ?? [setting.key]).join(' ');
  const shown = shownValue(setting);
  const rowLabel = label ?? copy.label;
  const rowHelp = help ?? helpOf(setting, copy.help);

  if (!setting.editable || setting.kind === LocalSettingKind.Structured) {
    return (
      <ReadOnlyRow
        keys={keys ?? [setting.key]}
        label={rowLabel}
        value={describeValue(setting)}
        help={help ?? setting.description ?? copy.help}
        tag={hideTag ? undefined : tagOf(setting)}
      />
    );
  }

  const control = setting.kind === LocalSettingKind.Boolean ? 'switch' : setting.kind === LocalSettingKind.Choice ? 'select' : 'text';
  const value = control === 'switch' ? Boolean(shown) : control === 'select' ? String(shown ?? '') : textOf(setting, shown);
  const options = (setting.options ?? []).map((option) => ({ value: option, label: optionLabel(setting.key, option) }));

  const commit = async (next: boolean | string) => {
    const saved = await updateSetting(setting.key, wireValue(setting, next));
    onSaved(saved);
    if (control === 'text' && textOf(saved, shownValue(saved)) === value && String(next) !== value) {
      setResync((count) => count + 1);
    }
  };

  return (
    <SettingRow
      key={resync}
      data-setting-keys={rowKeys}
      label={control === 'text' && typeof rowLabel !== 'string' ? copy.label : rowLabel}
      help={rowHelp}
      control={control}
      value={value}
      options={options}
      // A menu with one entry is shown, not offered.
      disabled={control === 'select' && options.length <= 1}
      tag={hideTag ? undefined : tagOf(setting)}
      onCommit={commit}
    />
  );
}

/** The help under a key: the catalog's words, and what applies when the file says otherwise. */
function helpOf(setting: LocalSetting, help: string | undefined): ReactNode {
  if (!overridden(setting)) return help;
  const note = `config.yaml says ${JSON.stringify(setting.value)}; ${describeValue(setting)} applies.`;
  return help ? `${help} ${note}` : note;
}
