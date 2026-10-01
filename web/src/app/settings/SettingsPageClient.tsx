'use client';

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Lock } from 'lucide-react';
import { LocalSettingKind, isHarness, type AutoPublishRule, type LocalSetting, type LocalSettingsResponse, type SyncAuthResponse } from '@peasant-labs/schema';
import { Button, Chip, CommandBlock, FeedbackPanel, ProviderName, SettingGroup, SettingRow, StatsStrip } from '@/lib/ft-ui';
import { fetchSettings, fetchVillageAuth, fetchVillageCollectives, updateSetting } from '@/lib/api/settings';
import {
  NOT_IN_PEASANT_CONFIG,
  RETIRED_KEYS,
  SETTINGS_GROUPS,
  describeValue,
  groupOf,
  optionLabel,
  shownValue,
  sourceHarnesses,
  tagOf,
  type SettingsGroupId,
} from '@/lib/settings/catalog';
import { ReadOnlyRow, SettingKeyRow } from '@/components/settings/SettingKeyRow';
import { VillageAccountRow } from '@/components/settings/VillageAccountRow';
import { CustomPatterns } from '@/components/settings/CustomPatterns';
import { AutoPublishRules, type CollectiveNames } from '@/components/settings/AutoPublishRules';
import './settings.css';

type Load = { state: 'loading' } | { state: 'failed'; error: string } | { state: 'ready'; data: LocalSettingsResponse };

/** A server description as a sentence: it ends with a period. */
function sentence(text: string): string {
  return /[.!?]$/.test(text.trim()) ? text.trim() : `${text.trim()}.`;
}

function messageOf(failure: unknown): string {
  return failure instanceof Error ? failure.message : String(failure);
}

/** How many projects and sessions the saved selection names, counted from the value the server sent. */
function selectionSummary(mode: LocalSetting | undefined, harnesses: LocalSetting | undefined): string {
  if (!mode || shownValue(mode) !== 'selected') return 'every project on this computer. ';
  const value = harnesses ? shownValue(harnesses) : null;
  let projects = 0;
  let sessions = 0;
  if (value && typeof value === 'object' && !Array.isArray(value)) {
    for (const harness of Object.values(value as Record<string, unknown>)) {
      if (!harness || typeof harness !== 'object') continue;
      const entry = harness as { projects?: unknown; sessions?: unknown };
      if (Array.isArray(entry.projects)) projects += entry.projects.length;
      if (Array.isArray(entry.sessions)) sessions += entry.sessions.length;
    }
  }
  const parts = [`${projects} ${projects === 1 ? 'project' : 'projects'}`];
  if (sessions > 0) parts.push(`${sessions} ${sessions === 1 ? 'session' : 'sessions'}`);
  return `${parts.join(' and ')} you picked. `;
}

/**
 * A group's content: its rows (a null row is a key the server did not send),
 * the notes under them, and what its summary counts when that is not the rows.
 */
interface GroupBody {
  rows: ReactNode[];
  notes?: ReactNode;
  count?: number;
  noun?: [string, string];
  description?: ReactNode;
}

/**
 * /settings: every peasant setting and the auto-publish rules, in groups.
 *
 * A control applies when it changes: one PATCH per change, pending then
 * settled; a refused change puts the value back and shows the server's
 * reason. The page decides nothing the server owns: the keys, kinds, menus,
 * what may change, what `peasant config` covers, and which repositories a rule
 * covers all come from the API.
 */
export default function SettingsPageClient() {
  const [load, setLoad] = useState<Load>({ state: 'loading' });
  const [auth, setAuth] = useState<SyncAuthResponse | null>(null);
  const [authError, setAuthError] = useState<string | null>(null);
  const [collectives, setCollectives] = useState<CollectiveNames>(null);
  const [collectivesError, setCollectivesError] = useState<string | null>(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);

  const readAuth = useCallback(async () => {
    setAuth(null);
    setAuthError(null);
    setCollectives(null);
    setCollectivesError(null);
    let signIn: SyncAuthResponse;
    try {
      signIn = await fetchVillageAuth();
    } catch (failure) {
      if (alive.current) setAuthError(messageOf(failure));
      return;
    }
    if (!alive.current) return;
    setAuth(signIn);
    setCollectives(null);
    setCollectivesError(null);
    if (!signIn.authenticated) return;
    try {
      const listed = await fetchVillageCollectives();
      if (alive.current) setCollectives(new Map(listed.collectives.map(({ group }) => [group.id, group.name])));
    } catch (failure) {
      if (alive.current) setCollectivesError(messageOf(failure));
    }
  }, []);

  const readSettings = useCallback(async () => {
    try {
      const data = await fetchSettings();
      if (alive.current) setLoad({ state: 'ready', data });
    } catch (failure) {
      if (alive.current) setLoad({ state: 'failed', error: messageOf(failure) });
    }
  }, []);

  useEffect(() => {
    void readSettings();
    void readAuth();
  }, [readSettings, readAuth]);

  const onSaved = useCallback((saved: LocalSetting) => {
    setLoad((current) => current.state !== 'ready' ? current : {
      state: 'ready',
      data: { ...current.data, settings: current.data.settings.map((setting) => (setting.key === saved.key ? saved : setting)) },
    });
  }, []);

  const onRulesChange = useCallback((rules: AutoPublishRule[]) => {
    setLoad((current) => current.state !== 'ready' ? current : { state: 'ready', data: { ...current.data, autoPublish: rules } });
  }, []);

  const onLoggedOut = useCallback(async () => {
    await Promise.all([readAuth(), readSettings()]);
  }, [readAuth, readSettings]);

  const settings = load.state === 'ready' ? load.data.settings : null;
  const byKey = useMemo(() => new Map((settings ?? []).map((setting) => [setting.key, setting])), [settings]);

  if (load.state !== 'ready') {
    return (
      <div className="iu-page stg-page">
        <PageHead />
        {load.state === 'loading' ? (
          <FeedbackPanel variant="loading" title="reading your settings" />
        ) : (
          <FeedbackPanel variant="error" title="the settings could not be read">
            <span className="stg-load-error">{load.error}</span>
            <Button variant="secondary" size="sm" onClick={() => { setLoad({ state: 'loading' }); void readSettings(); }}>try again</Button>
          </FeedbackPanel>
        )}
      </div>
    );
  }

  const rules = load.data.autoPublish;
  const publishing = rules.filter((rule) => rule.events.length > 0).length;
  const level = byKey.get('redaction.level');
  const summary = [
    auth?.authenticated
      ? { label: 'connected as', value: `@${auth.username ?? ''}`, order: 'label-first' as const }
      : { label: 'village', value: authError ? 'unavailable' : auth ? 'not connected' : 'reading', order: 'label-first' as const },
    publishing > 0
      ? { label: 'auto-publish on for', value: `${publishing} ${publishing === 1 ? 'rule' : 'rules'}`, order: 'label-first' as const }
      : { label: 'auto-publish', value: 'off', order: 'label-first' as const },
    ...(level ? [{ label: 'redaction', value: describeValue(level), order: 'label-first' as const }] : []),
  ];

  // Each group takes the keys it places; a key it does not place, and any key
  // this page has no place for, still renders as a plain row in its group.
  const consumed = new Set<string>();
  const take = (key: string): LocalSetting | undefined => {
    const setting = byKey.get(key);
    if (setting) consumed.add(key);
    return setting;
  };
  const row = (key: string, props: Partial<Parameters<typeof SettingKeyRow>[0]> = {}) => {
    const setting = take(key);
    return setting ? <SettingKeyRow key={key} setting={setting} onSaved={onSaved} {...props} /> : null;
  };
  const bodies: Record<SettingsGroupId, () => GroupBody> = {
    village: () => ({
      rows: [
        <VillageAccountRow key="account" auth={auth} readError={authError} onRetry={() => { void readAuth(); }} connected={take('village.connected')} onLoggedOut={onLoggedOut} />,
        row('push.sharePreference'),
      ],
    }),
    'auto-publish': () => ({
      count: rules.length,
      noun: ['rule', 'rules'],
      description: (
        <span className="stg-description">
          these folders and repositories publish redacted on their own. you agree once, here.
          <Chip size="sm" className="srow-tag">{NOT_IN_PEASANT_CONFIG}</Chip>
        </span>
      ),
      rows: [<AutoPublishRules key="rules" rules={rules} onRulesChange={onRulesChange} collectives={collectives} collectivesError={collectivesError} signedIn={auth?.authenticated === true} />],
    }),
    redaction: () => {
      const patterns = take('redaction.custom_patterns');
      return {
        rows: [
          row('redaction.level'),
          patterns ? <CustomPatterns key="patterns" setting={patterns} onSaved={onSaved} /> : null,
        ],
      };
    },
    projects: () => {
      const mode = take('selection.mode');
      const harnesses = take('selection.harnesses');
      const branches = take('selection.autoIngestNewBranches');
      const projectMode = take('daemon.projectMode');
      return {
        rows: [
          mode ? (
            <ReadOnlyRow
              key="selection"
              keys={['selection.mode', ...(harnesses ? ['selection.harnesses'] : [])]}
              label="record sessions from"
              value={optionLabel('selection.mode', String(shownValue(mode)))}
              help={`${selectionSummary(mode, harnesses)}the page shows the selection; peasant kickstart changes it.`}
              tag={tagOf(mode)}
              action={<CommandBlock command="peasant kickstart" label="peasant kickstart, which edits the selection" className="stg-command" />}
            />
          ) : null,
          branches ? (
            <ReadOnlyRow
              key="branches"
              keys={['selection.autoIngestNewBranches']}
              label="include new branches automatically"
              value={describeValue(branches)}
              help="only in projects you selected in full. part of the selection."
              tag={tagOf(branches)}
            />
          ) : null,
          // daemon.projectMode holds opt-in or opt-out; the switch reads on as opt-out.
          projectMode?.editable && projectMode.kind === LocalSettingKind.String ? (
            <SettingRow
              key="project-mode"
              data-setting-keys="daemon.projectMode"
              label="track new projects automatically"
              help={`saved as daemon.projectMode: ${String(shownValue(projectMode))}.`}
              control="switch"
              value={shownValue(projectMode) === 'opt-out'}
              tag={tagOf(projectMode)}
              onCommit={async (next) => { onSaved(await updateSetting(projectMode.key, next ? 'opt-out' : 'opt-in')); }}
            />
          ) : projectMode ? <SettingKeyRow key="project-mode" setting={projectMode} onSaved={onSaved} /> : null,
        ],
        notes: <p className="stg-note">this only filters lists and search. recorded sessions stay until you run peasant prune.</p>,
      };
    },
    sources: () => {
      const harnesses = sourceHarnesses(settings ?? []);
      const enabled = harnesses.filter((harness) => shownValue(byKey.get(`sources.${harness}.enabled`)!) === true).length;
      return {
        count: harnesses.length,
        noun: ['source', 'sources'],
        description: `where peasant reads sessions from. ${enabled} of ${harnesses.length} on.`,
        rows: harnesses.map((harness) => (
          <div key={harness} className="stg-source">
            {row(`sources.${harness}.enabled`, { label: isHarness(harness) ? <ProviderName harness={harness} /> : harness, help: `reads ${harness} sessions.` })}
            {/* the pair is one source: its tag sits on the switch */}
            {row(`sources.${harness}.paths`, { label: `${harness} folders`, help: 'separate folders with commas. empty uses the default.', hideTag: true })}
          </div>
        )),
      };
    },
    'fields-sent': () => ({
      noun: ['field', 'fields'],
      description: 'what peasant sends with each transcript, beside the transcript itself.',
      rows: ['push.fields.gitRemote', 'push.fields.projectName', 'push.fields.projectPath', 'push.fields.gitBranch', 'push.fields.hostSlug'].map((key) => row(key)),
      notes: <p className="stg-note">a salted project hash is always sent.</p>,
    }),
    advanced: () => {
      const mock = ['sources.mock.enabled', 'sources.mock.web', 'sources.mock.tui', 'sources.mock.api'].map(take).filter((setting): setting is LocalSetting => setting !== undefined);
      const basePath = take('output.basePath');
      const visibility = take('push.visibility');
      const license = take('push.license');
      const concurrency = byKey.get('push.concurrency');
      const hidden = [visibility, license].filter((setting): setting is LocalSetting => setting !== undefined);
      return {
        description: 'theme, email, village address, uploads, import folder, sample data.',
        rows: [
          row('display.theme'),
          row('user.email'),
          row('village.url'),
          row('push.method'),
          row('push.sources'),
          row('push.concurrency', concurrency ? { help: `empty uses half your cpu cores: ${describeValue(concurrency)} on this computer.` } : {}),
          row('output.stalenessThresholdSec'),
          basePath ? <ReadOnlyRow key="base-path" keys={[basePath.key]} label="import folder" value={describeValue(basePath)} help={basePath.description} tag={tagOf(basePath)} /> : null,
          mock.length > 0 ? (
            <ReadOnlyRow
              key="mock"
              keys={mock.map((setting) => setting.key)}
              label="sample data, for development"
              value={describeValue(mock[0])}
              help={mock[0].description}
              tag={tagOf(mock[0])}
            />
          ) : null,
          ...hidden.map((setting) => (
            <ReadOnlyRow
              key={setting.key}
              keys={[setting.key]}
              label={setting.key === 'push.visibility' ? 'default visibility' : 'default license'}
              value={describeValue(setting)}
              help="hidden while publishing is collectives only. kept in config.yaml."
              action={<Lock className="stg-lock" aria-label="hidden" />}
            />
          )),
        ],
      };
    },
    files: () => {
      const version = take('version');
      const retired = RETIRED_KEYS.map(take).filter((setting): setting is LocalSetting => setting !== undefined);
      return {
        description: 'where these settings are saved. an edit to a file shows here the next time the page loads.',
        rows: [
          <ReadOnlyRow
            key="config"
            keys={version ? [version.key] : []}
            label="settings"
            value={<code className="stg-code">config.yaml</code>}
            help={`in peasant's config folder: ~/.config/peasant, unless XDG_CONFIG_HOME or --config says otherwise.${version ? ` format version ${describeValue(version)}.` : ''}`}
          />,
          <ReadOnlyRow
            key="hooks"
            keys={[]}
            label="auto-publish rules"
            value={<code className="stg-code">hooks.yaml</code>}
            help="in the same config folder. peasant village auto and this page write it."
          />,
          retired.length > 0 ? (
            <ReadOnlyRow
              key="retired"
              keys={retired.map((setting) => setting.key)}
              label="retired keys"
              value={retired.every((setting) => setting.value === null) ? 'none set' : 'set in config.yaml'}
              help={(
                <ul className="stg-retired">
                  {retired.map((setting) => <li key={setting.key}><code>{setting.key}</code>: {sentence((setting.description ?? 'retired').replace(/^retired: /, ''))}</li>)}
                </ul>
              )}
            />
          ) : null,
        ],
      };
    },
  };

  return (
    <div className="iu-page stg-page">
      <PageHead />
      <StatsStrip items={summary} label="settings summary" />
      <div className="stg-groups">
        {SETTINGS_GROUPS.map((group) => {
          const body = bodies[group.id]();
          const rows = body.rows.filter((node) => node !== null && node !== undefined);
          // A key this page does not place renders in its group, after the rows the group places.
          const unplaced = (settings ?? []).filter((setting) => !consumed.has(setting.key) && groupOf(setting.key) === group.id);
          unplaced.forEach((setting) => consumed.add(setting.key));
          return (
            <SettingGroup
              key={group.id}
              data-group={group.id}
              label={group.label}
              defaultOpen={group.open}
              count={(body.count ?? rows.length) + unplaced.length}
              noun={body.noun}
              description={body.description}
            >
              {rows}
              {unplaced.map((setting) => <SettingKeyRow key={setting.key} setting={setting} onSaved={onSaved} />)}
              {body.notes}
            </SettingGroup>
          );
        })}
      </div>
    </div>
  );
}

function PageHead() {
  return (
    <header className="iu-page-head">
      <h1 className="iu-page-title">settings</h1>
      <p className="iu-page-sub">
        changes save right away, to config.yaml. anything tagged <Chip size="sm" className="srow-tag">{NOT_IN_PEASANT_CONFIG}</Chip> can be changed here, not in peasant config.
      </p>
    </header>
  );
}
