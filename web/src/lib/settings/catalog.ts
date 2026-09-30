import { LocalSettingKind, type LocalSetting } from '@peasant-labs/schema';

/**
 * The settings page's groups, in page order, and which start open.
 *
 * The server owns the keys, their kinds, their menus, and which ones may be
 * changed (GET /api/v1/settings). This module owns only where each key sits on
 * the page and the words next to it. A key the server serves that this map does
 * not name still renders, as a plain row in `advanced`; the page test fails for
 * it, so a new key gets a group on purpose.
 */
export const SETTINGS_GROUPS = [
  { id: 'village', label: 'village', open: true },
  { id: 'auto-publish', label: 'auto-publish', open: true },
  { id: 'redaction', label: 'redaction', open: true },
  { id: 'projects', label: 'projects', open: true },
  { id: 'sources', label: 'sources', open: false },
  { id: 'fields-sent', label: 'fields sent', open: false },
  { id: 'advanced', label: 'advanced', open: false },
  { id: 'files', label: 'files', open: false },
] as const;

export type SettingsGroupId = (typeof SETTINGS_GROUPS)[number]['id'];

/** The group a key the page does not place renders in. */
export const FALLBACK_GROUP: SettingsGroupId = 'advanced';

/**
 * Every key GET /api/v1/settings serves, and the group it shows in. The page
 * test holds this map to internal/api/testdata/settings-keys.yaml: a key there
 * with no entry here fails, and so does an entry for a key the server no
 * longer serves.
 */
export const SETTING_GROUP_OF: Readonly<Record<string, SettingsGroupId>> = {
  'village.connected': 'village',
  'push.sharePreference': 'village',

  'redaction.level': 'redaction',
  'redaction.custom_patterns': 'redaction',

  'selection.mode': 'projects',
  'selection.autoIngestNewBranches': 'projects',
  'selection.harnesses': 'projects',
  'daemon.projectMode': 'projects',

  'sources.claude-code.enabled': 'sources',
  'sources.claude-code.paths': 'sources',
  'sources.opencode.enabled': 'sources',
  'sources.opencode.paths': 'sources',
  'sources.codex.enabled': 'sources',
  'sources.codex.paths': 'sources',
  'sources.cursor.enabled': 'sources',
  'sources.cursor.paths': 'sources',
  'sources.strike.enabled': 'sources',
  'sources.strike.paths': 'sources',
  'sources.pi.enabled': 'sources',
  'sources.pi.paths': 'sources',

  'push.fields.gitRemote': 'fields-sent',
  'push.fields.projectName': 'fields-sent',
  'push.fields.projectPath': 'fields-sent',
  'push.fields.gitBranch': 'fields-sent',
  'push.fields.hostSlug': 'fields-sent',

  'display.theme': 'advanced',
  'user.email': 'advanced',
  'village.url': 'advanced',
  'push.method': 'advanced',
  'push.sources': 'advanced',
  'push.concurrency': 'advanced',
  'output.stalenessThresholdSec': 'advanced',
  'output.basePath': 'advanced',
  'sources.mock.enabled': 'advanced',
  'sources.mock.web': 'advanced',
  'sources.mock.tui': 'advanced',
  'sources.mock.api': 'advanced',
  'push.visibility': 'advanced',
  'push.license': 'advanced',

  'version': 'files',
  // Retired keys peasant still reads, to refuse or ignore them.
  'sources.claude.enabled': 'files',
  'sources.claude.paths': 'files',
  'selection.providers': 'files',
  'push.fields.projectHash': 'files',
};

/** The retired keys, listed together in `files` with how peasant treats each. */
export const RETIRED_KEYS: readonly string[] = ['sources.claude.enabled', 'sources.claude.paths', 'selection.providers', 'push.fields.projectHash'];

export function groupOf(key: string): SettingsGroupId {
  return SETTING_GROUP_OF[key] ?? FALLBACK_GROUP;
}

/** The words next to one key. Chrome is lowercase; a value keeps its case. */
export interface KeyCopy {
  label: string;
  help?: string;
  /** The words for each menu value of a choice key. A value with none shows as itself. */
  options?: Readonly<Record<string, string>>;
}

export const KEY_COPY: Readonly<Record<string, KeyCopy>> = {
  'push.sharePreference': {
    label: 'publishing plan',
    help: 'picked at setup. it never publishes on its own.',
    options: { '': 'keep local', 'share-later': 'publish later' },
  },
  'redaction.level': {
    label: 'level',
    help: 'hides secrets, pii, paths and project details. applies to every publish, by hand or automatic.',
  },
  'selection.mode': {
    label: 'record sessions from',
    options: { all: 'all projects', selected: 'selected projects' },
  },
  'selection.autoIngestNewBranches': {
    label: 'include new branches automatically',
    help: 'only in projects you selected in full.',
  },
  'daemon.projectMode': { label: 'track new projects automatically' },
  'push.fields.gitRemote': { label: 'repository label and git remote url' },
  'push.fields.projectName': { label: 'project name' },
  'push.fields.projectPath': { label: 'project path, if there is no repository label' },
  'push.fields.gitBranch': { label: 'branch name' },
  'push.fields.hostSlug': { label: 'host slug' },
  'display.theme': { label: 'terminal theme', help: 'the colors peasant uses in the terminal.' },
  'user.email': { label: 'your email', help: 'empty uses your git email.' },
  'village.url': { label: 'village address', help: 'where peasant publishes. empty uses the default.' },
  'push.method': {
    label: 'what peasant village push sends',
    options: { all: 'all sessions', 'by-source': 'by source' },
  },
  'push.sources': {
    label: 'sources for by source',
    help: 'the tools peasant village push reads when it sends by source. separate them with commas.',
  },
  'push.concurrency': { label: 'parallel uploads', help: 'empty uses half your cpu cores.' },
  'output.stalenessThresholdSec': { label: 're-import after, in seconds' },
  'output.basePath': { label: 'import folder' },
  'push.visibility': { label: 'default visibility' },
  'push.license': { label: 'default license', options: { '': 'none' } },
};

/** The copy for a key, or the key itself for one this page has no words for. */
export function copyOf(key: string): KeyCopy {
  return KEY_COPY[key] ?? { label: key };
}

/** The words for one menu value of a choice key. */
export function optionLabel(key: string, option: string): string {
  return KEY_COPY[key]?.options?.[option] ?? (option === '' ? 'not set' : option);
}

/**
 * The value a row shows: what applies (`effective`), so an unset key never
 * reads as off or blank when its default is on or filled. It falls back to the
 * file's value only when nothing applies.
 */
export function shownValue(setting: LocalSetting): unknown {
  return setting.effective ?? setting.value ?? null;
}

/** A key's value as words, for a row that shows it without a control. */
export function describeValue(setting: LocalSetting): string {
  const value = shownValue(setting);
  if (value === null || value === undefined) return 'not set';
  switch (setting.kind) {
    case LocalSettingKind.Boolean:
      return value ? 'on' : 'off';
    case LocalSettingKind.Choice:
      return optionLabel(setting.key, String(value));
    case LocalSettingKind.StringList:
      return Array.isArray(value) && value.length > 0 ? value.join(', ') : 'none';
    case LocalSettingKind.Structured:
      if (Array.isArray(value)) return value.length === 0 ? 'none' : `${value.length} ${value.length === 1 ? 'entry' : 'entries'}`;
      if (value && typeof value === 'object') {
        const size = Object.keys(value).length;
        return size === 0 ? 'none' : `${size} ${size === 1 ? 'entry' : 'entries'}`;
      }
      return String(value);
    default:
      return String(value) === '' ? 'not set' : String(value);
  }
}

/** Whether the file names a value that differs from the one that applies. */
export function overridden(setting: LocalSetting): boolean {
  if (setting.value === null || setting.value === undefined) return false;
  return JSON.stringify(setting.value) !== JSON.stringify(setting.effective);
}

/** The tag on a key that `peasant config` cannot change. */
export const NOT_IN_PEASANT_CONFIG = 'not in peasant config';

/** The tag a key's row carries, read from the server's `inPeasantConfig`. */
export function tagOf(setting: LocalSetting): string | undefined {
  return setting.inPeasantConfig ? undefined : NOT_IN_PEASANT_CONFIG;
}

/** A source harness's keys: `sources.<harness>.enabled` and `.paths`. */
export function sourceHarnesses(settings: readonly LocalSetting[]): string[] {
  const harnesses: string[] = [];
  for (const setting of settings) {
    const match = /^sources\.([^.]+)\.enabled$/.exec(setting.key);
    // The retired and development sources are read-only; they are not tools to read from.
    if (match && setting.editable && match[1] !== 'mock') harnesses.push(match[1]);
  }
  return harnesses;
}
