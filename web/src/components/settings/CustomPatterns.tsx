'use client';

import { useState, type FormEvent } from 'react';
import { ArrowRight, CircleCheck, CircleX, Loader, MoreHorizontal, Plus } from 'lucide-react';
import type { LocalSetting } from '@peasant-labs/schema';
import { Button, Chip, Input, Menu, Select } from '@/lib/ft-ui';
import { updateSetting } from '@/lib/api/settings';
import { tagOf } from '@/lib/settings/catalog';
import type { OnSettingSaved } from './SettingKeyRow';

/** One entry of redaction.custom_patterns, as config.yaml holds it. */
export interface CustomPattern {
  id: string;
  category: string;
  pattern: string;
  replacement: string;
}

/**
 * The categories a pattern may name. The redaction engine owns them and the
 * server refuses any other; the page shows each one as it is stored.
 */
const PATTERN_CATEGORIES = ['secrets', 'pii', 'paths', 'project'] as const;

function patternsOf(setting: LocalSetting): CustomPattern[] {
  const value = setting.effective ?? setting.value;
  if (!Array.isArray(value)) return [];
  return value.flatMap((entry) => {
    if (!entry || typeof entry !== 'object') return [];
    const record = entry as Record<string, unknown>;
    return [{
      id: String(record.id ?? ''),
      category: String(record.category ?? ''),
      pattern: String(record.pattern ?? ''),
      replacement: String(record.replacement ?? ''),
    }];
  });
}

const EMPTY: CustomPattern = { id: '', category: 'secrets', pattern: '', replacement: '' };

type Status = { state: 'idle' | 'pending' | 'settled' } | { state: 'failed'; error: string };

/**
 * redaction.custom_patterns: your own redaction rules, listed, added, edited
 * and removed here. Each change writes the whole list in one PATCH; a refused
 * change leaves the list as it was and shows the server's reason.
 */
export function CustomPatterns({ setting, onSaved }: { setting: LocalSetting; onSaved: OnSettingSaved }) {
  const patterns = patternsOf(setting);
  const [editing, setEditing] = useState<{ index: number | null; draft: CustomPattern } | null>(null);
  const [status, setStatus] = useState<Status>({ state: 'idle' });
  const busy = status.state === 'pending';

  const write = async (next: CustomPattern[]): Promise<boolean> => {
    if (busy) return false;
    setStatus({ state: 'pending' });
    try {
      const saved = await updateSetting(setting.key, next.length === 0 ? null : next);
      onSaved(saved);
      setStatus({ state: 'settled' });
      return true;
    } catch (failure) {
      setStatus({ state: 'failed', error: failure instanceof Error ? failure.message : String(failure) });
      return false;
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!editing || busy) return;
    const draft = { ...editing.draft, id: editing.draft.id.trim() };
    const next = editing.index === null
      ? [...patterns, draft]
      : patterns.map((pattern, index) => (index === editing.index ? draft : pattern));
    if (await write(next)) setEditing(null);
  };

  const field = (name: keyof CustomPattern) => ({
    value: editing?.draft[name] ?? '',
    onChange: (event: { target: { value: string } }) => setEditing((current) => current && { ...current, draft: { ...current.draft, [name]: event.target.value } }),
    disabled: busy,
  });

  const tag = tagOf(setting);

  return (
    <div className="stg-block" data-setting-keys={setting.key}>
      <div className="stg-block-head">
        <span className="srow-label-line">
          <span className="srow-label">your own patterns</span>
          {tag && <Chip size="sm" className="srow-tag">{tag}</Chip>}
        </span>
        {setting.editable && editing === null && (
          <Button variant="secondary" size="sm" icon={Plus} disabled={busy} onClick={() => { if (busy) return; setStatus({ state: 'idle' }); setEditing({ index: null, draft: EMPTY }); }}>add a pattern</Button>
        )}
      </div>

      {patterns.length === 0 ? (
        <p className="stg-empty">no patterns yet. the built-in rules still apply.</p>
      ) : (
        <ul className="stg-patterns" aria-label="your own patterns">
          {patterns.map((pattern, index) => (
            <li key={`${pattern.id}-${index}`} className="stg-pattern">
              <span className="stg-pattern-id">{pattern.id}</span>
              <span className="stg-pattern-category">{pattern.category}</span>
              <span className="stg-pattern-rule">
                <code className="stg-code">{pattern.pattern}</code>
                <ArrowRight className="stg-pattern-arrow" role="img" aria-label="becomes" />
                {pattern.replacement ? <code className="stg-code">{pattern.replacement}</code> : <span className="stg-muted">removed</span>}
              </span>
              {setting.editable && (
                <Menu
                  icon={MoreHorizontal}
                  ariaLabel={`more for ${pattern.id}`}
                  align="end"
                  size="sm"
                  items={[
                    { label: 'edit', disabled: busy, onSelect: () => { if (busy) return; setStatus({ state: 'idle' }); setEditing({ index, draft: pattern }); } },
                    { label: 'remove', danger: true, disabled: busy, onSelect: () => { void write(patterns.filter((_, at) => at !== index)); } },
                  ]}
                />
              )}
            </li>
          ))}
        </ul>
      )}

      {editing && (
        <form className="stg-form" onSubmit={submit} aria-label={editing.index === null ? 'add a pattern' : `edit ${editing.draft.id || 'pattern'}`}>
          <div className="stg-form-grid">
            <Input label="name" required placeholder="acme-internal-host" {...field('id')} />
            <Select
              label="category"
              value={editing.draft.category}
              onChange={(event) => field('category').onChange(event)}
              disabled={busy}
              options={PATTERN_CATEGORIES.map((category) => ({ value: category, label: category }))}
            />
            {/* a pattern and its replacement are code: mono, never lowercased */}
            <div className="stg-mono-field"><Input label="pattern (a regular expression)" required placeholder="[a-z0-9-]+\.acme\.internal" {...field('pattern')} /></div>
            <div className="stg-mono-field"><Input label="replace with" placeholder="<INTERNAL_HOST>" hint="empty removes each match." {...field('replacement')} /></div>
          </div>
          <div className="stg-form-actions">
            <Button type="submit" variant="primary" size="sm" disabled={busy}>save pattern</Button>
            <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => { setEditing(null); setStatus({ state: 'idle' }); }}>cancel</Button>
          </div>
        </form>
      )}

      <StatusLine status={status} settledWord="saved" />
    </div>
  );
}

/** The pending, settled or failed mark of a block that saves as a whole. */
export function StatusLine({ status, settledWord, pendingWord = 'saving', failedWord = 'not saved' }: {
  status: Status;
  settledWord: string;
  pendingWord?: string;
  failedWord?: string;
}) {
  return (
    <div className="stg-status-line" data-status={status.state}>
      <span className={`srow-status srow-status-${status.state}`} aria-live="polite">
        {status.state === 'pending' && <><Loader className="srow-status-icon srow-spin" aria-hidden="true" /><span>{pendingWord}</span></>}
        {status.state === 'settled' && <><CircleCheck className="srow-status-icon" aria-hidden="true" /><span>{settledWord}</span></>}
        {status.state === 'failed' && <><CircleX className="srow-status-icon" aria-hidden="true" /><span>{failedWord}</span></>}
      </span>
      {status.state === 'failed' && <span className="srow-error" role="alert">{status.error}</span>}
    </div>
  );
}

export type { Status as BlockStatus };
