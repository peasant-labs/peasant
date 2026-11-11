'use client';

import { useCallback, useMemo, useState, type MouseEvent, type FocusEvent } from 'react';
import { Button, FeedbackPanel, Progress, SelectionNodeKind, SelectionTree, Skeleton } from '@/lib/ft-ui';
import type { KickstartField } from '@/lib/kickstart/model';
import type { KickstartSessionApi } from '@/lib/kickstart/useKickstartSession';
import { useKickstartPreview } from '@/lib/kickstart/usePreview';
import { scanPercent, selectedIds } from '@/lib/kickstart/view';
import { KickstartScanState } from '@/lib/kickstart/values';
import { ExampleLines } from './PrivacyStep';

/**
 * The selection stop: the server's forest in fairtrade's SelectionTree, the
 * discovery job's progress, and a preview of the row the reader is on.
 *
 * The tree draws each row's `state` and `locked` as the server sent them and
 * is given the server's selected ids as `value`; a toggle sends the ids it
 * reports back as the session's answer. Nothing here derives membership.
 */

const countFormat = new Intl.NumberFormat('en-US');

/** The rows of a forest that has not been read: one array, so the tree is not handed a new list each render. */
const NO_ROWS: readonly never[] = [];

/** The id of the forest row an event came from, read from the row the tree draws. */
function rowOf(target: EventTarget | null): string | null {
  if (!(target instanceof Element)) return null;
  return target.closest('[data-node-id]')?.getAttribute('data-node-id') ?? null;
}

function ScanProgress({ api }: { api: KickstartSessionApi }) {
  const scan = api.scan;
  if (scan === null) return null;
  if (scan.state === KickstartScanState.Error) {
    return (
      <FeedbackPanel variant="error" title="the scan did not finish">
        <span>{scan.error ?? 'peasant gave no reason. scan again; if it repeats, check the peasant log.'}</span>
        <Button variant="secondary" size="sm" onClick={() => void api.rescan()}>
          scan again
        </Button>
      </FeedbackPanel>
    );
  }
  if (scan.state === KickstartScanState.Running || scan.state === KickstartScanState.Idle) {
    const percent = scanPercent(scan);
    return (
      <div className="ks-scan" role="status">
        <Progress
          value={percent ?? 0}
          showPct={percent !== null}
          ariaLabel="scan progress"
          label={scan.phase ?? scan.state.label}
        />
        {scan.total > 0 && (
          <span className="ks-counts">
            <span className="ks-num">{countFormat.format(scan.done)}</span> of <span className="ks-num">{countFormat.format(scan.total)}</span>
          </span>
        )}
      </div>
    );
  }
  return null;
}

export function SelectionStep({ field, api }: { field: KickstartField; api: KickstartSessionApi }) {
  const { tree, scan, session } = api;
  const [highlighted, setHighlighted] = useState<string | null>(null);
  const preview = useKickstartPreview(session?.id ?? null, highlighted);

  // The rows are the contract's own, validated where they were read; the tree
  // draws their kind and state, so the page names neither.
  const rows = tree === null ? NO_ROWS : tree.nodes;
  const selection = useMemo(() => {
    try {
      return { ids: selectedIds(field), error: null };
    } catch (failure) {
      return { ids: null, error: failure instanceof Error ? failure.message : String(failure) };
    }
  }, [field]);

  const onSelectionChange = useCallback((ids: readonly string[]) => void api.selectIds(ids), [api]);
  const onRowEvent = useCallback((event: MouseEvent<HTMLDivElement> | FocusEvent<HTMLDivElement>) => {
    const id = rowOf(event.target);
    if (id !== null) setHighlighted(id);
  }, []);

  const sessions = rows.filter((node) => node.kind === SelectionNodeKind.Session.wire).length;

  return (
    <div className="ks-field">
      {field.description !== null && <p className="ks-help">{field.description}</p>}
      <ScanProgress api={api} />
      {scan !== null && scan.droppedSelections > 0 && (
        <p className="ks-help" role="status">
          <span className="ks-num">{countFormat.format(scan.droppedSelections)}</span> saved{' '}
          {scan.droppedSelections === 1 ? 'choice is' : 'choices are'} no longer on disk
          {scan.droppedLabels.length > 0 ? `: ${scan.droppedLabels.join(', ')}` : ''}.
        </p>
      )}
      {scan !== null && scan.lockedNodes > 0 && (
        <p className="ks-help" role="status">
          <span className="ks-num">{countFormat.format(scan.lockedNodes)}</span>{' '}
          {scan.lockedNodes === 1 ? 'row is' : 'rows are'} locked because a saved rule is ambiguous. locked rows are shown and cannot be changed here.
        </p>
      )}
      {api.treeError !== null && (
        <FeedbackPanel variant="error" title="the sessions could not be listed">
          <span>{api.treeError.message}</span>
          <Button variant="secondary" size="sm" onClick={() => void api.reloadTree()}>
            try again
          </Button>
        </FeedbackPanel>
      )}
      {selection.error !== null && (
        <FeedbackPanel variant="error" title="the selection cannot be shown">
          <span>{selection.error}</span>
        </FeedbackPanel>
      )}
      {tree === null && api.treeError === null && <Skeleton label="listing your sessions" lines={3} />}
      {tree !== null && selection.ids !== null && (
        <div className="ks-select">
          {rows.length === 0 ? (
            <FeedbackPanel variant="empty" title="no sessions found">
              <span>peasant found no recorded sessions on this computer. scan again after you use an agent.</span>
            </FeedbackPanel>
          ) : (
            <div className="ks-tree" onClick={onRowEvent} onFocus={onRowEvent}>
              <SelectionTree nodes={rows} value={selection.ids} onSelectionChange={onSelectionChange} ariaLabel="sessions to import" />
            </div>
          )}
          <aside className="ks-preview" aria-label="preview">
            <p className="ks-preview-title">preview</p>
            {highlighted === null && <p className="ks-help">choose a row to read the start of it before you import it.</p>}
            {preview.loading && <Skeleton label="reading the preview" avatar={false} lines={2} />}
            {preview.error !== null && <p className="ks-refusal" role="alert">{preview.error.message}</p>}
            {preview.lines !== null && (preview.lines.length === 0
              ? <p className="ks-help">this row has nothing to preview.</p>
              : <ExampleLines lines={preview.lines} label="preview lines" />)}
          </aside>
        </div>
      )}
      {tree !== null && rows.length > 0 && (
        <p className="ks-counts">
          <span className="ks-num">{countFormat.format(sessions)}</span> {sessions === 1 ? 'session' : 'sessions'} found
          {selection.ids !== null && (
            <>
              , <span className="ks-num">{countFormat.format(selection.ids.length)}</span> chosen
            </>
          )}
        </p>
      )}
      {(scan === null || scan.state !== KickstartScanState.Error) && (
        <div className="ks-answers">
          <Button variant="ghost" size="sm" onClick={() => void api.rescan()}>
            scan again
          </Button>
        </div>
      )}
      {field.refusal !== null && (
        <p className="ks-refusal" role="alert">
          {field.refusal.error}
        </p>
      )}
    </div>
  );
}
