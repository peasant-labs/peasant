'use client';

import { useState, type FormEvent } from 'react';
import { CircleCheck, MoreHorizontal, Plus, TriangleAlert } from 'lucide-react';
import {
  AutoPublishEvent,
  AutoPublishHookStatus,
  AutoPublishRuleKind,
  type AutoPublishHook,
  type AutoPublishRepository,
  type AutoPublishRule,
} from '@peasant-labs/schema';
import { Button, Checkbox, CopyIconButton, Input, Menu, Select, SettingRow } from '@/lib/ft-ui';
import { installAutoPublishRule, removeAutoPublishRule, saveAutoPublishRule } from '@/lib/api/settings';
import { shellQuote } from '@/lib/settings/shell';
import { StatusLine, type BlockStatus } from './CustomPatterns';

/** The collectives this computer's Village account can see, by identifier. null: not signed in or not read. */
export type CollectiveNames = ReadonlyMap<string, string> | null;

function eventWords(event: AutoPublishEvent): string {
  return event === AutoPublishEvent.PostCommit ? 'on each commit' : 'on git push';
}

function kindWords(kind: AutoPublishRuleKind): string {
  return kind === AutoPublishRuleKind.Remote ? 'git remote' : 'folder';
}

/** A repository the rule covers whose hook is not there yet: installing is offered for it. */
function needsHook(repository: AutoPublishRepository): boolean {
  return repository.hooks.some((hook) => hook.status === AutoPublishHookStatus.Absent);
}

/** One install the page offers: one call to a rule's install endpoint. */
export interface PendingInstall {
  /** The rule whose endpoint makes the call. */
  ruleId: string;
  match: string;
  path: string;
  label?: string;
  /** The events this call installs: the calling rule's event set. */
  events: AutoPublishEvent[];
  /** Every rule whose repository view this call's result refreshes. */
  ruleIds: string[];
}

/** A stable key for an event set, so two rules with the same events share one call. */
function eventSetKey(events: readonly AutoPublishEvent[]): string {
  return [...events].sort().join(',');
}

/**
 * The installs the one action makes: every recorded repository a rule covers
 * that has no hook yet. Identical work collapses: two rules whose event sets
 * match over one repository install once, and both rule views refresh from the
 * result. Different event sets over one repository still take one call each:
 * the install endpoint is rule-scoped, so a call installs only the calling
 * rule's events. Covering disjoint events in one call needs a schema-owned
 * union operation; until then this stays one call per event set. A repository
 * whose hook is there or blocked is not offered, and neither is one a paused
 * rule covers: the server reports one hook per rule event, so a paused rule's
 * repositories have none.
 */
export function pendingInstalls(rules: readonly AutoPublishRule[]): PendingInstall[] {
  const operations = new Map<string, PendingInstall>();
  for (const rule of rules) {
    for (const repository of rule.repositories) {
      if (!needsHook(repository)) continue;
      const key = `${repository.path}\u0000${eventSetKey(rule.events)}`;
      const existing = operations.get(key);
      if (existing) {
        existing.ruleIds.push(rule.id);
        continue;
      }
      operations.set(key, {
        ruleId: rule.id,
        match: rule.match,
        path: repository.path,
        label: repository.label,
        events: [...rule.events],
        ruleIds: [rule.id],
      });
    }
  }
  return [...operations.values()];
}

function repositoryName(repository: { path: string; label?: string }): string {
  return repository.label ? repository.label.replace(/^[^:]+:/, '') : repository.path;
}

/**
 * The labels more than one repository in a list shares. A repository is named
 * by its remote label; its folder shows only when there is no label or the
 * label does not tell it apart.
 */
function sharedLabels(repositories: readonly { label?: string }[]): Set<string> {
  const seen = new Set<string>();
  const shared = new Set<string>();
  for (const { label } of repositories) {
    if (!label) continue;
    if (seen.has(label)) shared.add(label);
    seen.add(label);
  }
  return shared;
}

function RepositoryName({ repository, shared }: { repository: { path: string; label?: string }; shared: ReadonlySet<string> }) {
  const showPath = repository.label !== undefined && repository.label !== '' && shared.has(repository.label);
  return (
    <span className="stg-repo-id" title={repository.path}>
      <span className="stg-repo-name">{repositoryName(repository)}</span>
      {showPath && <span className="stg-repo-path">{repository.path}</span>}
    </span>
  );
}

function collectiveWords(ids: readonly string[], names: CollectiveNames): string {
  if (ids.length === 0) return 'no collective';
  if (!names) return `${ids.length} ${ids.length === 1 ? 'collective' : 'collectives'}`;
  return ids.map((id) => names.get(id) ?? `a collective you cannot see (${id.slice(0, 8)})`).join(', ');
}

function hookSummary(rule: AutoPublishRule): string {
  const total = rule.repositories.length;
  if (total === 0) return 'no recorded repository matches yet';
  const installed = rule.repositories.filter((repository) => repository.hooks.length > 0 && repository.hooks.every((hook) => hook.status === AutoPublishHookStatus.Installed)).length;
  const blocked = rule.repositories.filter((repository) => repository.hooks.some((hook) => hook.status === AutoPublishHookStatus.Blocked)).length;
  const parts = [`hook in ${installed} of ${total} ${total === 1 ? 'repository' : 'repositories'}`];
  if (blocked > 0) parts.push(`blocked in ${blocked}`);
  return parts.join(' · ');
}

function hookWords(status: AutoPublishHookStatus): string {
  switch (status) {
    case AutoPublishHookStatus.Installed: return 'hook installed';
    case AutoPublishHookStatus.Blocked: return 'blocked';
    case AutoPublishHookStatus.Failed: return 'install failed';
    default: return 'no hook yet';
  }
}

/** A blocked or failed hook's remedy: what to do, and the section to add by hand when there is one. */
function Remedy({ hook }: { hook: AutoPublishHook }) {
  if (!hook.remedy) return null;
  return (
    <div className="stg-remedy">
      <p className="stg-remedy-text">{hook.remedy.message}</p>
      {hook.remedy.snippet && (
        <div className="stg-snippet">
          <pre className="stg-snippet-code"><code>{hook.remedy.snippet}</code></pre>
          <CopyIconButton value={hook.remedy.snippet} label={`copy the ${hook.event} section`} />
        </div>
      )}
    </div>
  );
}

function RepositoryState({ repository, shared }: { repository: AutoPublishRepository; shared: ReadonlySet<string> }) {
  return (
    <li className="stg-repo">
      <RepositoryName repository={repository} shared={shared} />
      {repository.hooks.length === 0 ? (
        <span className="stg-repo-state">paused</span>
      ) : repository.hooks.map((hook) => (
        <span key={hook.event} className={`stg-repo-state stg-repo-state-${hook.status}`}>
          {repository.hooks.length > 1 ? `${hook.event}: ` : ''}{hookWords(hook.status)}
        </span>
      ))}
      {repository.hooks.map((hook) => <Remedy key={`${hook.event}-remedy`} hook={hook} />)}
    </li>
  );
}

/**
 * The command that removes the hook an install writes; a single event names it.
 * The path is quoted so a directory with a space or a quote survives the shell;
 * the event is a closed-set token and stays bare, as the Go renderer prints it.
 */
function uninstallCommand(path: string, events: readonly AutoPublishEvent[]): string {
  const command = `peasant village hooks uninstall --dir ${shellQuote(path)}`;
  return events.length === 1 ? `${command} --event ${events[0]}` : command;
}

/**
 * What installing in one repository will do, shown before the action: the
 * events and the collectives they publish to, the managed hook's failure
 * behavior, the uninstall command, and any blocked hook's remedy. The effective
 * hook path and the exact bound command are not on the wire yet; they stay a
 * follow-up rather than being reconstructed here.
 */
function InstallPreview({ operations, rules, collectives }: {
  operations: readonly PendingInstall[];
  rules: readonly AutoPublishRule[];
  collectives: CollectiveNames;
}) {
  const byId = new Map(rules.map((rule) => [rule.id, rule]));
  const steps = new Map<string, { event: AutoPublishEvent; to: readonly string[] }>();
  const remedies = new Map<string, AutoPublishHook>();
  for (const operation of operations) {
    for (const ruleId of operation.ruleIds) {
      const rule = byId.get(ruleId);
      if (!rule) continue;
      for (const event of rule.events) {
        const key = `${event}\u0000${[...rule.collectives].sort().join(',')}`;
        if (!steps.has(key)) steps.set(key, { event, to: rule.collectives });
      }
      const repository = rule.repositories.find((candidate) => candidate.path === operation.path);
      for (const hook of repository?.hooks ?? []) {
        if (hook.remedy) remedies.set(`${ruleId}\u0000${hook.event}`, hook);
      }
    }
  }
  return (
    <details className="stg-install-preview">
      <summary>what installing does</summary>
      <ul className="stg-install-steps">
        {[...steps.entries()].map(([key, step]) => (
          <li key={key} className="stg-install-step">
            {eventWords(step.event)} · publishes to {collectiveWords(step.to, collectives)}, private
          </li>
        ))}
      </ul>
      <p className="stg-note">the managed hook always exits 0, so a failed upload never blocks the push or commit.</p>
      {operations.map((operation) => (
        <p key={`${operation.ruleId}-${operation.path}`} className="stg-note">
          remove it with <code className="stg-command">{uninstallCommand(operation.path, operation.events)}</code>.
        </p>
      ))}
      {[...remedies.entries()].map(([key, hook]) => <Remedy key={key} hook={hook} />)}
    </details>
  );
}

interface RuleDraft {
  id: string;
  isNew: boolean;
  kind: AutoPublishRuleKind;
  match: string;
  /** No events keeps a paused rule paused. */
  events: AutoPublishEvent[];
  collectives: string[];
}

/** The two events a rule may publish on, in the order the form shows them. */
const RULE_EVENTS: readonly AutoPublishEvent[] = [AutoPublishEvent.PrePush, AutoPublishEvent.PostCommit];

function newRuleId(): string {
  const bytes = new Uint8Array(6);
  crypto.getRandomValues(bytes);
  return `rule-${Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')}`;
}

/** The result of one install call: the repository as it is now, or why the call failed. */
type InstallOutcome = PendingInstall & ({ repository: AutoPublishRepository } | { error: string });

/** One event's install state in a repository, however many calls touched it. */
interface InstallEventResult {
  event: AutoPublishEvent;
  /** The hook a successful call reported for this event. */
  hook?: AutoPublishHook;
  /** Why a failed call could not install this event. */
  error?: string;
}

/** One repository's final install result, however many calls covered it. */
interface InstallResult {
  path: string;
  label?: string;
  /** The events the calls touched, in the order they first appeared. */
  events: InstallEventResult[];
}

/** The repositories whose every event the install left installed. */
function installedCount(outcomes: readonly InstallOutcome[]): number {
  return installResults(outcomes).filter((result) =>
    result.events.length > 0 &&
    result.events.every((entry) => entry.error === undefined && entry.hook?.status === AutoPublishHookStatus.Installed)
  ).length;
}

/**
 * Collapse the per-call outcomes into one result per repository, by event. The
 * install endpoint is rule-scoped, so a call answers only for the calling
 * rule's events: two rules covering disjoint events over one repository each
 * contribute their own hooks, and merging by event keeps them all instead of
 * letting the last call hide the first. A call that failed leaves its events
 * failed until a later call succeeds for that same event; a success for another
 * event never clears the failure.
 */
function installResults(outcomes: readonly InstallOutcome[]): InstallResult[] {
  const byPath = new Map<string, { result: InstallResult; events: Map<AutoPublishEvent, InstallEventResult> }>();
  for (const outcome of outcomes) {
    const entry = byPath.get(outcome.path) ?? {
      result: { path: outcome.path, label: outcome.label, events: [] },
      events: new Map<AutoPublishEvent, InstallEventResult>(),
    };
    const eventOf = (event: AutoPublishEvent): InstallEventResult => {
      const existing = entry.events.get(event);
      if (existing) return existing;
      const created: InstallEventResult = { event };
      entry.events.set(event, created);
      entry.result.events.push(created);
      return created;
    };
    if ('error' in outcome) {
      for (const event of outcome.events) eventOf(event).error = outcome.error;
    } else {
      for (const hook of outcome.repository.hooks) {
        const event = eventOf(hook.event);
        event.hook = hook;
        event.error = undefined;
      }
    }
    byPath.set(outcome.path, entry);
  }
  return [...byPath.values()].map((entry) => entry.result);
}

/**
 * The auto-publish rules in hooks.yaml. A rule binds a folder or a git remote
 * to collectives; saving one installs nothing. The recorded repositories a
 * publishing rule covers that have no hook yet are listed, with one action
 * that installs in all of them, only after the click.
 */
export function AutoPublishRules({ rules, onRulesChange, collectives, collectivesError = null, signedIn }: {
  rules: readonly AutoPublishRule[];
  onRulesChange: (rules: AutoPublishRule[]) => void;
  collectives: CollectiveNames;
  /** Why the collectives could not be read, when signed in. */
  collectivesError?: string | null;
  signedIn: boolean;
}) {
  const [draft, setDraft] = useState<RuleDraft | null>(null);
  const [formStatus, setFormStatus] = useState<BlockStatus>({ state: 'idle' });
  const [listStatus, setListStatus] = useState<BlockStatus>({ state: 'idle' });
  const [switchPending, setSwitchPending] = useState(false);
  const [removalNote, setRemovalNote] = useState<string | null>(null);
  const [install, setInstall] = useState<{ running: boolean; done: number; total: number; outcomes: InstallOutcome[] } | null>(null);
  // Rule and install responses describe the whole rules snapshot captured by
  // their action. Serialize mutations so a late response cannot restore consent
  // or replace another rule decision made while it was pending.
  const busy = install?.running === true || switchPending || listStatus.state === 'pending' || formStatus.state === 'pending';
  const pending = pendingInstalls(rules);
  const pendingRepos = new Set(pending.map((item) => item.path)).size;
  const pendingByPath = new Map<string, PendingInstall[]>();
  for (const item of pending) {
    const group = pendingByPath.get(item.path);
    if (group) group.push(item);
    else pendingByPath.set(item.path, [item]);
  }
  const outcomes = install?.outcomes ?? [];
  const resultRepos = new Set(outcomes.map((outcome) => outcome.path)).size;
  const currentOperation = install?.running ? pending[install.done] : undefined;

  const replaceRule = (saved: AutoPublishRule) => {
    const exists = rules.some((rule) => rule.id === saved.id);
    onRulesChange(exists ? rules.map((rule) => (rule.id === saved.id ? saved : rule)) : [...rules, saved]);
  };

  const put = (rule: AutoPublishRule, change: Partial<Pick<AutoPublishRule, 'events' | 'collectives' | 'kind' | 'match'>>) =>
    saveAutoPublishRule(rule.id, {
      kind: change.kind ?? rule.kind,
      match: change.match ?? rule.match,
      events: change.events ?? rule.events,
      collectives: change.collectives ?? rule.collectives,
    });

  const setPublishing = async (rule: AutoPublishRule, on: boolean) => {
    if (busy) return;
    setSwitchPending(true);
    try {
      replaceRule(await put(rule, { events: on ? [AutoPublishEvent.PrePush] : [] }));
    } finally {
      setSwitchPending(false);
    }
  };

  const switchEvent = async (rule: AutoPublishRule, event: AutoPublishEvent) => {
    if (busy) return;
    setListStatus({ state: 'pending' });
    try {
      replaceRule(await put(rule, { events: [event] }));
      setListStatus({ state: 'settled' });
    } catch (failure) {
      setListStatus({ state: 'failed', error: failure instanceof Error ? failure.message : String(failure) });
    }
  };

  const remove = async (rule: AutoPublishRule) => {
    if (busy) return;
    setListStatus({ state: 'pending' });
    setRemovalNote(null);
    try {
      const removed = await removeAutoPublishRule(rule.id);
      onRulesChange(rules.filter((existing) => existing.id !== rule.id));
      setListStatus({ state: 'settled' });
      const kept = removed.repositories.filter((repository) => repository.hooks.some((hook) => hook.status === AutoPublishHookStatus.Installed)).length;
      setRemovalNote(kept === 0
        ? `removed ${rule.match}.`
        : `removed ${rule.match}. its hook stays in ${kept} ${kept === 1 ? 'repository' : 'repositories'} as files. rule-required hooks publish only if another active binding covers the repository and event. separately installed terminal hooks keep their own consent.`);
    } catch (failure) {
      setListStatus({ state: 'failed', error: failure instanceof Error ? failure.message : String(failure) });
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!draft || busy) return;
    setFormStatus({ state: 'pending' });
    try {
      const saved = await saveAutoPublishRule(draft.id, {
        kind: draft.kind,
        match: draft.match.trim(),
        events: draft.events,
        collectives: draft.collectives,
      });
      replaceRule(saved);
      setDraft(null);
      setFormStatus({ state: 'idle' });
    } catch (failure) {
      setFormStatus({ state: 'failed', error: failure instanceof Error ? failure.message : String(failure) });
    }
  };

  const runInstall = async () => {
    if (busy) return;
    const plan = pending;
    setInstall({ running: true, done: 0, total: plan.length, outcomes: [] });
    const outcomes: InstallOutcome[] = [];
    let current = [...rules];
    for (const item of plan) {
      try {
        const repository = await installAutoPublishRule(item.ruleId, item.path);
        outcomes.push({ ...item, repository });
        // Every rule this operation covered refreshes from the one result.
        current = current.map((rule) => (item.ruleIds.includes(rule.id) ? {
          ...rule,
          repositories: rule.repositories.map((existing) => (existing.path === repository.path ? repository : existing)),
        } : rule));
      } catch (failure) {
        outcomes.push({ ...item, error: failure instanceof Error ? failure.message : String(failure) });
      }
      setInstall({ running: true, done: outcomes.length, total: plan.length, outcomes: [...outcomes] });
    }
    onRulesChange(current);
    setInstall({ running: false, done: outcomes.length, total: plan.length, outcomes });
  };

  const openForm = (rule?: AutoPublishRule) => {
    if (busy) return;
    setFormStatus({ state: 'idle' });
    setDraft(rule
      ? { id: rule.id, isNew: false, kind: rule.kind, match: rule.match, events: [...rule.events], collectives: [...rule.collectives] }
      : { id: newRuleId(), isNew: true, kind: AutoPublishRuleKind.Folder, match: '', events: [AutoPublishEvent.PrePush], collectives: [] });
  };

  const collectiveChoices = collectives ? Array.from(collectives.entries()) : [];

  return (
    <div className="stg-autopublish">
      {rules.length === 0 ? (
        <p className="stg-empty">no folder or repository publishes on its own. add one below, or tick auto-publish the next time you publish.</p>
      ) : (
        <div className="stg-rules">
          {rules.map((rule) => {
            const event = rule.events[0];
            const other: AutoPublishEvent = event === AutoPublishEvent.PostCommit ? AutoPublishEvent.PrePush : AutoPublishEvent.PostCommit;
            return (
              <div key={rule.id} className="stg-rule" data-rule-id={rule.id}>
                <SettingRow
                  label={rule.match}
                  help={`${kindWords(rule.kind)} · ${rule.events.length === 0 ? 'paused, publishes nothing' : `publishes to ${collectiveWords(rule.collectives, collectives)} ${rule.events.map(eventWords).join(' and ')}`}`}
                  control="switch"
                  value={rule.events.length > 0}
                  disabled={busy}
                  onCommit={(next) => setPublishing(rule, Boolean(next))}
                />
                <div className="stg-rule-foot">
                  {rule.repositories.length === 0 ? (
                    <span className="stg-rule-repos-none">{hookSummary(rule)}</span>
                  ) : (
                    <details className="stg-rule-repos">
                      <summary>{hookSummary(rule)}</summary>
                      <ul className="stg-repos">
                        {rule.repositories.map((repository) => <RepositoryState key={repository.path} repository={repository} shared={sharedLabels(rule.repositories)} />)}
                      </ul>
                    </details>
                  )}
                  <Menu
                    icon={MoreHorizontal}
                    ariaLabel={`more for ${rule.match}`}
                    align="end"
                    size="sm"
                    items={[
                      { label: 'edit', disabled: busy, onSelect: () => openForm(rule) },
                      ...(event ? [{ label: `publish ${eventWords(other)} instead`, disabled: busy, onSelect: () => { void switchEvent(rule, other); } }] : []),
                      { label: 'remove', danger: true, disabled: busy, onSelect: () => { void remove(rule); } },
                    ]}
                  />
                </div>
              </div>
            );
          })}
        </div>
      )}
      <StatusLine status={listStatus} settledWord="saved" />
      {removalNote && <p className="stg-note" role="status">{removalNote}</p>}

      {(pending.length > 0 || install) && (
        <section className="stg-install" aria-labelledby="stg-install-title">
          <h3 id="stg-install-title" className="stg-install-title">
            {pending.length > 0
              ? <><span className="tnum">{pendingRepos}</span> recorded {pendingRepos === 1 ? 'repository matches' : 'repositories match'} a rule and {pendingRepos === 1 ? 'has' : 'have'} no hook yet</>
              : `installed in ${installedCount(outcomes)} of ${resultRepos} ${resultRepos === 1 ? 'repository' : 'repositories'}`}
          </h3>
          {pending.length > 0 && (
            <>
              <ul className="stg-repos" aria-label="repositories to install in">
                {[...pendingByPath.values()].map((operations) => {
                  const first = operations[0];
                  const matches = [...new Set(operations.map((operation) => operation.match))];
                  return (
                    <li key={first.path} className="stg-repo">
                      <RepositoryName repository={first} shared={sharedLabels(pending)} />
                      <span className="stg-repo-state">for {matches.join(', ')}</span>
                      <InstallPreview operations={operations} rules={rules} collectives={collectives} />
                    </li>
                  );
                })}
              </ul>
              <p className="stg-note">nothing is installed until you choose to. peasant never overwrites a hook it did not write.</p>
              <div className="stg-install-actions">
                <Button variant="primary" onClick={runInstall} disabled={busy} loading={install?.running === true}>
                  install in {pendingRepos} {pendingRepos === 1 ? 'repository' : 'repositories'}
                </Button>
                {install?.running && (
                  <span className="stg-progress" aria-live="polite">
                    {currentOperation ? `installing ${repositoryName(currentOperation)} (${currentOperation.events.map(eventWords).join(' and ')}) ` : 'installing '}
                    <span className="tnum">{install.done + 1}</span> of <span className="tnum">{install.total}</span>
                  </span>
                )}
              </div>
            </>
          )}
          {install && !install.running && (
            <ul className="stg-repos stg-install-results" aria-label="install results">
              {installResults(outcomes).map((result) => {
                const multiple = result.events.length > 1;
                return (
                  <li key={result.path} className="stg-repo">
                    <RepositoryName repository={result} shared={sharedLabels(outcomes)} />
                    {result.events.map((entry) => {
                      const status = entry.error !== undefined ? AutoPublishHookStatus.Failed : entry.hook?.status;
                      if (status === undefined) return null;
                      return (
                        <span key={entry.event} className={`stg-repo-state stg-repo-state-${status}`}>
                          {status === AutoPublishHookStatus.Installed ? <CircleCheck aria-hidden="true" /> : <TriangleAlert aria-hidden="true" />}
                          {multiple ? ` ${entry.event}: ` : ' '}{hookWords(status)}
                        </span>
                      );
                    })}
                    {result.events.map((entry) => entry.error !== undefined ? (
                      <div key={`${entry.event}-error`} className="stg-remedy">
                        <p className="stg-remedy-text" role="alert">{entry.error}</p>
                      </div>
                    ) : entry.hook ? <Remedy key={`${entry.event}-remedy`} hook={entry.hook} /> : null)}
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      )}

      {draft ? (
        <form className="stg-form" onSubmit={submit} aria-label={draft.isNew ? 'add a folder or repository' : `edit ${draft.match}`}>
          <div className="stg-form-grid">
            <Select
              label="what it matches"
              value={draft.kind}
              onChange={(event) => setDraft({ ...draft, kind: event.target.value as AutoPublishRuleKind })}
              disabled={busy}
              options={[{ value: AutoPublishRuleKind.Folder, label: 'a folder' }, { value: AutoPublishRuleKind.Remote, label: 'a git remote' }]}
            />
            <div className="stg-mono-field">
              <Input
                label={kindWords(draft.kind)}
                required
                placeholder={draft.kind === AutoPublishRuleKind.Remote ? 'github.com:acme/*' : '~/work/acme/**'}
                hint={draft.kind === AutoPublishRuleKind.Remote ? 'a remote, or a pattern such as github.com:acme/*.' : 'a folder, or a glob such as ~/work/acme/**.'}
                value={draft.match}
                onChange={(event) => setDraft({ ...draft, match: event.target.value })}
                disabled={busy}
              />
            </div>
          </div>
          <fieldset className="stg-events" disabled={busy}>
            <legend className="label">publish</legend>
            {RULE_EVENTS.map((event) => (
              <Checkbox
                key={event}
                checked={draft.events.includes(event)}
                onChange={(checked) => setDraft({ ...draft, events: checked ? [...draft.events, event] : draft.events.filter((existing) => existing !== event) })}
              >
                {eventWords(event)}
              </Checkbox>
            ))}
            <p className="stg-note">both off keeps this rule paused: it publishes nothing.</p>
          </fieldset>
          <fieldset className="stg-collectives" disabled={busy}>
            <legend className="label">publish to</legend>
            {!signedIn ? (
              <p className="stg-note">sign in to village to pick collectives: run peasant village login, or sign in from the publish popup.</p>
            ) : collectivesError !== null ? (
              <p className="stg-note" role="alert">the collectives could not be read from village: {collectivesError}</p>
            ) : collectives === null ? (
              <p className="stg-note">reading your collectives from village.</p>
            ) : collectiveChoices.length === 0 ? (
              <p className="stg-note">this account is in no collective yet. join or create one on village first.</p>
            ) : collectiveChoices.map(([id, name]) => (
              <Checkbox
                key={id}
                checked={draft.collectives.includes(id)}
                onChange={(checked) => setDraft({ ...draft, collectives: checked ? [...draft.collectives, id] : draft.collectives.filter((existing) => existing !== id) })}
              >
                {name}
              </Checkbox>
            ))}
          </fieldset>
          <p className="stg-note">sessions recorded where this rule matches publish redacted to these collectives, with no review. saving installs no hook.</p>
          <div className="stg-form-actions">
            <Button type="submit" variant="primary" size="sm" disabled={busy || draft.match.trim() === '' || draft.collectives.length === 0}>save rule</Button>
            <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => { setDraft(null); setFormStatus({ state: 'idle' }); }}>cancel</Button>
          </div>
          <StatusLine status={formStatus} settledWord="saved" />
        </form>
      ) : (
        <div className="stg-add">
          <Button variant="secondary" icon={Plus} disabled={busy} onClick={() => openForm()}>add a folder or repository</Button>
        </div>
      )}
    </div>
  );
}
