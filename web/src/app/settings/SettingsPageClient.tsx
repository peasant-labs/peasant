'use client';

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useEnter } from '@/lib/motion/enter';
import Link from 'next/link';
import { ExternalLink, EyeOff, Folder, Lock, Plug, Send, SlidersHorizontal, UserRound, type LucideIcon } from 'lucide-react';
import { LocalSettingKind, isHarness, type AutoPublishRule, type LocalSetting, type LocalSettingsResponse, type SyncAuthResponse } from '@peasant-labs/schema';
import { Button, FeedbackPanel, ProviderName, SettingGroup, SettingRow, SettingsLayout } from '@/lib/ft-ui';
import { fetchSettings, fetchVillageAuth, fetchVillageCollectives, updateSetting } from '@/lib/api/settings';
import {
  RETIRED_KEYS,
  SETTINGS_GROUPS,
  SETTINGS_SECTIONS,
  describeValue,
  groupOf,
  optionLabel,
  shownValue,
  sourceHarnesses,
  settingsSection,
  settingsSectionHref,
  type SettingsGroupId,
  type SettingsSection,
  type SettingsSectionId,
} from '@/lib/settings/catalog';
import { ReadOnlyRow, SettingKeyRow } from '@/components/settings/SettingKeyRow';
import { DocsLink } from '@/components/DocsLink';
import { VillageAccountRow } from '@/components/settings/VillageAccountRow';
import { CustomPatterns } from '@/components/settings/CustomPatterns';
import { AutoPublishRules, type CollectiveNames } from '@/components/settings/AutoPublishRules';
import { RedactionCategories } from '@/components/settings/RedactionCategories';
import { onlineCollectivesHref } from '@/lib/village/links';
import './settings.css';

/** The glyph each settings section leads with in the section bar. */
const SECTION_ICONS: Readonly<Record<SettingsSectionId, LucideIcon>> = {
  account: UserRound,
  'automatic-publishing': Send,
  redaction: EyeOff,
  projects: Folder,
  sources: Plug,
  advanced: SlidersHorizontal,
};

/** The section bar's entries: one route per section, in the catalog's order. */
const NAV_SECTIONS = SETTINGS_SECTIONS.map((entry) => ({
  id: entry.id,
  label: entry.label,
  href: settingsSectionHref(entry.id),
  icon: SECTION_ICONS[entry.id],
}));

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
 * /settings: every peasant setting and the auto-publish rules, in sections.
 * `section` picks the one the page shows (`/settings?section=<id>`); the
 * section links switch between them without reading the settings again.
 *
 * A control applies when it changes: one PATCH per change, pending then
 * settled; a refused change puts the value back and shows the server's
 * reason. The page decides nothing the server owns: the keys, kinds, menus,
 * what may change, what `peasant config` covers, and which repositories a rule
 * covers all come from the API.
 */
export default function SettingsPageClient({ section: sectionId }: { section?: string | null } = {}) {
  const section = settingsSection(sectionId);
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

  // A section switch keeps the page: its body fades and rises into place instead.
  const sectionBody = useRef<HTMLDivElement>(null);
  useEnter(sectionBody, section.id);

  const settings = load.state === 'ready' ? load.data.settings : null;
  const byKey = useMemo(() => new Map((settings ?? []).map((setting) => [setting.key, setting])), [settings]);

  if (load.state !== 'ready') {
    return (
      <SettingsFrame section={section}>
        <div ref={sectionBody} className="stg-body">
          <PageHead section={section} />
          {load.state === 'loading' ? (
            <FeedbackPanel variant="loading" title="reading your settings" />
          ) : (
            <FeedbackPanel variant="error" title="the settings could not be read">
              <span className="stg-load-error">{load.error}</span>
              <Button variant="secondary" size="sm" onClick={() => { setLoad({ state: 'loading' }); void readSettings(); }}>try again</Button>
            </FeedbackPanel>
          )}
        </div>
      </SettingsFrame>
    );
  }

  const rules = load.data.autoPublish;

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
    village: () => {
      // The sign-in row stands for village.connected, so the key is placed here.
      take('village.connected');
      return {
        rows: [
          <VillageAccountRow key="account" auth={auth} readError={authError} onRetry={() => { void readAuth(); }} onLoggedOut={onLoggedOut} />,
          auth?.authenticated ? (
            <div key="collectives" className="srow">
              <span className="srow-text-col">
                <span className="srow-label">your collectives</span>
                <span className="srow-help">who you publish to. you join and manage them in your dashboard.</span>
              </span>
              <a className="btn btn-secondary btn-sm" href={onlineCollectivesHref()} target="_blank" rel="noreferrer">
                <ExternalLink aria-hidden="true" />dashboard
              </a>
            </div>
          ) : null,
          row('push.sharePreference'),
          row('push.autoPublishIntent'),
        ],
      };
    },
    'auto-publish': () => ({
      count: rules.length,
      noun: ['rule', 'rules'],
      description: (
        <span className="stg-description">
          these folders and repositories publish redacted on their own. you agree once, here.
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
              help={selectionSummary(mode, harnesses).trim()}
              action={<DocsLink topic="selection">change the selection</DocsLink>}
            />
          ) : null,
          // Part of the selection, so it changes with it, not here: the switch
          // shows the value and stays still.
          branches ? (
            <SettingRow
              key="branches"
              data-setting-keys="selection.autoIngestNewBranches"
              label="include new branches automatically"
              help="only in projects you selected in full. part of the selection."
              control="switch"
              value={shownValue(branches) === true}
              disabled
              onCommit={() => {}}
            />
          ) : null,
          // daemon.projectMode holds opt-in or opt-out; the switch reads on as opt-out.
          projectMode?.editable && projectMode.kind === LocalSettingKind.String ? (
            <SettingRow
              key="project-mode"
              data-setting-keys="daemon.projectMode"
              label="track new projects automatically"
              control="switch"
              value={shownValue(projectMode) === 'opt-out'}
              onCommit={async (next) => { onSaved(await updateSetting(projectMode.key, next ? 'opt-out' : 'opt-in')); }}
            />
          ) : projectMode ? <SettingKeyRow key="project-mode" setting={projectMode} onSaved={onSaved} /> : null,
        ],
        notes: <p className="stg-note">this only filters lists and search; recorded sessions stay. <DocsLink topic="prune" variant="inline">remove recorded sessions</DocsLink></p>,
      };
    },
    sources: () => {
      const harnesses = sourceHarnesses(settings ?? []);
      return {
        count: harnesses.length,
        noun: ['source', 'sources'],
        rows: harnesses.map((harness) => (
          <div key={harness} className="stg-source">
            {row(`sources.${harness}.enabled`, { label: isHarness(harness) ? <ProviderName harness={harness} /> : harness, help: '' })}
            {row(`sources.${harness}.paths`, { label: `${harness} folders`, help: 'separate folders with commas. empty uses the default.' })}
          </div>
        )),
      };
    },
    'fields-sent': () => ({
      noun: ['field', 'fields'],
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
        rows: [
          row('display.theme'),
          row('user.email'),
          row('village.url'),
          row('push.method'),
          row('push.sources'),
          row('push.concurrency', concurrency ? { help: `empty uses half your cpu cores: ${describeValue(concurrency)} on this computer.` } : {}),
          row('output.stalenessThresholdSec'),
          basePath ? <ReadOnlyRow key="base-path" keys={[basePath.key]} label="import folder" value={describeValue(basePath)} help={basePath.description} /> : null,
          mock.length > 0 ? (
            <ReadOnlyRow
              key="mock"
              keys={mock.map((setting) => setting.key)}
              label="sample data, for development"
              value={describeValue(mock[0])}
              help={mock[0].description}
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
        rows: [
          <ReadOnlyRow
            key="config"
            keys={version ? [version.key] : []}
            label="settings"
            value={<code className="stg-code">config.yaml</code>}
            help={<>in peasant&apos;s config folder.{version ? ` format version ${describeValue(version)}.` : ''} <DocsLink topic="settingsFiles" variant="inline">where it lives</DocsLink></>}
          />,
          <ReadOnlyRow
            key="hooks"
            keys={[]}
            label="auto-publish rules"
            value={<code className="stg-code">hooks.yaml</code>}
            help="in the same config folder. this page writes it."
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

  const single = section.groups.length === 1;
  return (
    <SettingsFrame section={section}>
      <div ref={sectionBody} className="stg-body">
        <PageHead section={section} />
        {section.id === 'redaction' && <RedactionCategories />}
        <div className="stg-groups">
          {section.groups.map((id) => {
            const group = SETTINGS_GROUPS.find((candidate) => candidate.id === id)!;
            const body = bodies[group.id]();
            const rows = body.rows.filter((node) => node !== null && node !== undefined);
            // A key this page does not place renders in its group, after the rows the group places.
            const unplaced = (settings ?? []).filter((setting) => !consumed.has(setting.key) && groupOf(setting.key) === group.id);
            unplaced.forEach((setting) => consumed.add(setting.key));
            const content = (
              <>
                {rows}
                {unplaced.map((setting) => <SettingKeyRow key={setting.key} setting={setting} onSaved={onSaved} />)}
                {body.notes}
              </>
            );
            // A section that shows one group is that group: the page title names it.
            if (single) {
              return (
                <div key={group.id} className="srow-group stg-panel" data-group={group.id}>
                  {body.description && <p className="srow-group-description">{body.description}</p>}
                  <div className="srow-rows">{content}</div>
                </div>
              );
            }
            return (
              <SettingGroup
                key={group.id}
                data-group={group.id}
                label={group.label}
                defaultOpen
                count={(body.count ?? rows.length) + unplaced.length}
                noun={body.noun}
                description={body.description}
              >
                {content}
              </SettingGroup>
            );
          })}
        </div>
      </div>
    </SettingsFrame>
  );
}

function NextLink({ href, ...rest }: { href: string } & React.AnchorHTMLAttributes<HTMLAnchorElement>) {
  return <Link href={href} {...rest} />;
}

/**
 * The settings page, as fairtrade's demo and the online app lay it out: the
 * page's one title, settings, on the page edge, then fairtrade's in-page
 * section bar beside the section on a wide body, above it on a narrow one,
 * with the section's name heading its column. The app sidebar keeps its own
 * nav with the settings gear current; the bar is the one way between the
 * sections.
 */
function SettingsFrame({ section, children }: { section: SettingsSection; children: ReactNode }) {
  return (
    <div className="asb-page stg-page" data-settings-section={section.id}>
      <header className="iu-page-head">
        <h1 className="iu-page-title page-title">settings</h1>
      </header>
      <SettingsLayout sections={NAV_SECTIONS} currentId={section.id} LinkComponent={NextLink} label="settings sections">
        {children}
      </SettingsLayout>
    </div>
  );
}

function PageHead({ section }: { section: SettingsSection }) {
  return (
    <header className="stg-head">
      <h2 className="stg-title">{section.label}</h2>
    </header>
  );
}
