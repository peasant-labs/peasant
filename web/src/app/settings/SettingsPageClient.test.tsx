import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StrictMode } from 'react';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { AutoPublishRule, LocalSettingsResponse, SyncAuthResponse } from '@peasant-labs/schema';
import SettingsPageClient from './SettingsPageClient';
import { SETTING_GROUP_OF } from '@/lib/settings/catalog';
import { SETTING_ROW_STATES } from '@/lib/ft-ui';
import {
  loadInstallCases,
  loadPendingInstallCases,
  loadPendingRuleWriteCases,
  loadPendingPatternWriteCases,
  loadAuthRecoveryCases,
  loadServerKeyNames,
  loadSettingWrites,
  loadSettingsGroups,
  loadSettingsResponse,
} from '@/test/fixtures/settings';

/**
 * The settings page, mounted on the Local API answers the real server gives.
 * `fetch` is the one dependency replaced: every call the page makes goes
 * through the production API client and is recorded here.
 */

interface Answer { status: number; body: unknown }
type Route = (body: unknown, path: string) => Answer | Promise<Answer>;

interface Call { method: string; path: string; body: unknown }

let routes: Map<string, Route>;
let calls: Call[];

function answer(status: number, body: unknown): Answer {
  return { status, body };
}

/** Answers `method path`; a path ending in `/*` answers every path under it. */
function serve(method: string, path: string, route: Route) {
  routes.set(`${method} ${path}`, route);
}

function routeFor(method: string, path: string): Route | undefined {
  const exact = routes.get(`${method} ${path}`);
  if (exact) return exact;
  for (const [pattern, route] of routes) {
    const [verb, prefix] = pattern.split(' ');
    if (verb === method && prefix.endsWith('/*') && path.startsWith(prefix.slice(0, -1))) return route;
  }
  return undefined;
}

function mountServer({ settings, auth = { authenticated: false } }: { settings: LocalSettingsResponse; auth?: SyncAuthResponse }) {
  routes = new Map();
  calls = [];
  serve('GET', '/api/v1/settings', () => answer(200, settings));
  serve('GET', '/api/v1/sync/auth', () => answer(200, auth));
  serve('GET', '/api/v1/village/collectives', () => answer(200, {
    collectives: [
      { group: villageGroup('3f9c1a2b-0000-4000-8000-000000000001', 'Acme Platform') },
      { group: villageGroup('3f9c1a2b-0000-4000-8000-000000000002', 'Acme Labs') },
    ],
  }));
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(input);
    const method = init?.method ?? 'GET';
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, path: url.pathname, body });
    const route = routeFor(method, url.pathname);
    const reply = route ? await route(body, url.pathname) : answer(404, { error: `no route ${method} ${url.pathname}` });
    return { ok: reply.status >= 200 && reply.status < 300, status: reply.status, text: async () => JSON.stringify(reply.body) };
  }));
}

function villageGroup(id: string, name: string) {
  return {
    id,
    name,
    acceptance_mode: 'open',
    created_at: '2026-01-01T00:00:00Z',
    created_by: id,
    data_access: 'members_only',
    description: null,
    display_members: true,
    linked_github_org: null,
    member_count: 4,
    member_since: '2026-01-01T00:00:00Z',
    role: 'member',
    transcript_count: 9,
    transcript_deletion_policy: 'user_choice',
    updated_at: '2026-01-01T00:00:00Z',
  };
}

function withRules(rules: AutoPublishRule[]): LocalSettingsResponse {
  return { ...loadSettingsResponse(), autoPublish: rules };
}

async function mountPage() {
  render(<SettingsPageClient />);
  await screen.findByRole('heading', { name: 'settings' });
  await waitFor(() => expect(document.querySelector('details.srow-group')).not.toBeNull());
}

function rowOf(key: string): HTMLElement {
  const rows = document.querySelectorAll<HTMLElement>(`[data-setting-keys~="${key}"]`);
  expect(rows, `the row for ${key}`).toHaveLength(1);
  return rows[0];
}

function mutations(): Call[] {
  return calls.filter((call) => call.method !== 'GET');
}

beforeEach(() => {
  mountServer({ settings: loadSettingsResponse() });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('settings page groups', () => {
  for (const testCase of loadAuthRecoveryCases()) {
    it(`recovers sign-in reads in StrictMode: ${testCase.name}`, async () => {
      let recovered = false;
      let loggedOut = false;
      serve('GET', '/api/v1/sync/auth', () => {
        if (testCase.afterLogout && !loggedOut) return answer(200, { authenticated: true, username: 'alice-dev', villageUrl: 'https://api.village.peasantlabs.org' });
        return recovered ? answer(200, { authenticated: false }) : answer(testCase.status, { error: testCase.error });
      });
      serve('POST', '/api/v1/sync/logout', () => { loggedOut = true; return answer(200, { status: 'logged_out' }); });
      render(<StrictMode><SettingsPageClient /></StrictMode>);
      if (testCase.afterLogout) {
        await waitFor(() => expect(within(rowOf('village.connected')).getByText('@alice-dev')).toBeInTheDocument());
        fireEvent.click(within(rowOf('village.connected')).getByRole('button', { name: 'log out' }));
      }
      expect(await screen.findByRole('button', { name: 'retry sign-in' })).toBeVisible();
      expect(screen.getByRole('alert')).toHaveTextContent(testCase.error);
      expect(document.querySelector('details.srow-group')).not.toBeNull();
      expect(screen.queryAllByText('@alice-dev')).toHaveLength(0);
      expect(screen.queryByRole('button', { name: 'log out' })).toBeNull();
      recovered = true;
      fireEvent.click(screen.getByRole('button', { name: 'retry sign-in' }));
      await waitFor(() => expect(screen.queryByRole('button', { name: 'retry sign-in' })).toBeNull());
      expect(rowOf('village.connected')).toHaveTextContent('not connected');
      expect(screen.queryAllByText('@alice-dev')).toHaveLength(0);
      expect(mutations().map(({ method, path }) => `${method} ${path}`)).toEqual(testCase.afterLogout ? ['POST /api/v1/sync/logout'] : []);
    });
  }

  it('shows exactly the manifest groups, in order, open or collapsed as listed', async () => {
    const groups = loadSettingsGroups();
    await mountPage();
    const mounted = Array.from(document.querySelectorAll<HTMLDetailsElement>('details.srow-group')).map((group) => ({
      name: group.querySelector('.srow-summary-label')?.textContent,
      open: group.open,
    }));
    expect(mounted).toEqual(groups.map(({ name, open }) => ({ name, open })));
  });

  it('shows every key the server serves in exactly one group, the one the page names for it', async () => {
    const serverKeys = loadServerKeyNames();
    const sample = loadSettingsResponse();
    // The sample answer and the page's map both track the server's manifest.
    expect(new Set(sample.settings.map((setting) => setting.key))).toEqual(new Set(serverKeys));
    const unplaced = serverKeys.filter((key) => !(key in SETTING_GROUP_OF));
    expect(unplaced, 'keys the settings page has no group for').toEqual([]);
    const stale = Object.keys(SETTING_GROUP_OF).filter((key) => !serverKeys.includes(key));
    expect(stale, 'grouped keys the server no longer serves').toEqual([]);

    await mountPage();
    for (const key of serverKeys) {
      const shown = document.querySelectorAll(`[data-setting-keys~="${key}"]`);
      expect(shown, `${key} is shown once`).toHaveLength(1);
      const group = shown[0].closest<HTMLElement>('details.srow-group');
      expect(group?.dataset.group, `${key}'s group`).toBe(SETTING_GROUP_OF[key]);
    }
  });

  it('still shows a key it has no group for, as a plain row in advanced', async () => {
    const settings = loadSettingsResponse();
    settings.settings.push({ key: 'kickstart.intent', kind: 'string', value: null, effective: 'review', editable: true, inPeasantConfig: false });
    mountServer({ settings });
    await mountPage();
    expect(rowOf('kickstart.intent').closest<HTMLElement>('details.srow-group')?.dataset.group).toBe('advanced');
  });

  it('tags a row "not in peasant config" exactly when the server says peasant config cannot change it', async () => {
    const settings = loadSettingsResponse();
    // The page reads the flag; it does not keep its own list.
    const theme = settings.settings.find((setting) => setting.key === 'display.theme')!;
    theme.inPeasantConfig = true;
    mountServer({ settings });
    await mountPage();
    for (const setting of settings.settings.filter((candidate) => candidate.editable)) {
      // A source's folders row shares the tag its switch carries.
      const holder = rowOf(setting.key).closest<HTMLElement>('.stg-source') ?? rowOf(setting.key);
      const tagged = within(holder).queryAllByText('not in peasant config').length > 0;
      expect(tagged, `${setting.key} tagged`).toBe(!setting.inPeasantConfig);
    }
  });
});

describe('settings page writes', () => {
  it.each(loadSettingWrites().map((testCase) => [testCase.name, testCase] as const))('%s', async (_name, testCase) => {
    expect(SETTING_ROW_STATES).toContain(testCase.expect.status);
    let release: () => void = () => {};
    if (testCase.answer && 'hold' in testCase.answer) {
      serve('PATCH', '/api/v1/settings', () => new Promise<Answer>((resolve) => { release = () => resolve(answer(500, { error: 'released' })); }));
    } else if (testCase.answer) {
      const { status, body } = testCase.answer;
      serve('PATCH', '/api/v1/settings', () => answer(status, body));
    }
    await mountPage();
    const row = rowOf(testCase.key);
    row.closest('details')!.open = true;

    if (testCase.input.toggle) fireEvent.click(within(row).getByRole('switch'));
    if (testCase.input.select !== undefined) fireEvent.change(within(row).getByRole('combobox'), { target: { value: testCase.input.select } });
    if (testCase.input.type !== undefined) {
      fireEvent.click(within(row).getByRole('button', { name: /^edit / }));
      fireEvent.change(within(row).getByRole('textbox'), { target: { value: testCase.input.type } });
      fireEvent.click(within(row).getByRole('button', { name: 'save' }));
    }

    const current = () => rowOf(testCase.key);
    await waitFor(() => expect(current().getAttribute('data-status') ?? 'idle').toBe(testCase.expect.status));
    expect(mutations().map((call) => call.body)).toEqual(testCase.expect.request === null ? [] : [testCase.expect.request]);
    const control = within(current()).queryByRole('switch');
    if (control) expect(control.getAttribute('aria-checked')).toBe(testCase.expect.shown);
    else if (within(current()).queryByRole('combobox')) expect((within(current()).getByRole('combobox') as HTMLSelectElement).value).toBe(testCase.expect.shown);
    else expect(current().querySelector('.srow-text-value')?.textContent).toBe(testCase.expect.shown);
    const alert = within(current()).queryByRole('alert');
    expect(alert?.textContent ?? null).toBe(testCase.expect.alert);
    release();
  });

  it('reads the saved row back into the summary', async () => {
    serve('PATCH', '/api/v1/settings', () => answer(200, { key: 'push.sharePreference', kind: 'choice', value: 'share-later', effective: 'share-later', options: ['', 'share-later'], editable: true, inPeasantConfig: true }));
    await mountPage();
    fireEvent.change(within(rowOf('push.sharePreference')).getByRole('combobox'), { target: { value: 'share-later' } });
    await waitFor(() => expect(rowOf('push.sharePreference').getAttribute('data-status')).toBe('settled'));
    expect((within(rowOf('push.sharePreference')).getByRole('combobox') as HTMLSelectElement).value).toBe('share-later');
  });
});

describe('settings page village and projects', () => {
  it('shows the sign-in and logs out through POST /api/v1/sync/logout', async () => {
    let signedIn = true;
    mountServer({ settings: loadSettingsResponse(), auth: { authenticated: true, username: 'alice-dev', villageUrl: 'https://api.village.peasantlabs.org' } });
    serve('GET', '/api/v1/sync/auth', () => answer(200, signedIn ? { authenticated: true, username: 'alice-dev', villageUrl: 'https://api.village.peasantlabs.org' } : { authenticated: false }));
    serve('POST', '/api/v1/sync/logout', () => { signedIn = false; return answer(200, { status: 'logged_out' }); });
    await mountPage();
    const row = rowOf('village.connected');
    await within(row).findByText('@alice-dev');
    expect(mutations()).toEqual([]);
    fireEvent.click(within(row).getByRole('button', { name: 'log out' }));
    await within(rowOf('village.connected')).findByText('not connected');
    expect(mutations().map(({ method, path }) => `${method} ${path}`)).toEqual(['POST /api/v1/sync/logout']);
  });

  it('keeps the sign-in when logging out fails, and shows why', async () => {
    mountServer({ settings: loadSettingsResponse(), auth: { authenticated: true, username: 'alice-dev' } });
    serve('POST', '/api/v1/sync/logout', () => answer(500, { error: 'Village sign-out could not remove the stored credential. This computer is still signed in.', code: 'sync_logout_failed' }));
    await mountPage();
    const row = rowOf('village.connected');
    fireEvent.click(await within(row).findByRole('button', { name: 'log out' }));
    expect(await within(rowOf('village.connected')).findByRole('alert')).toHaveTextContent('This computer is still signed in.');
    expect(within(rowOf('village.connected')).getByText('@alice-dev')).toBeInTheDocument();
  });

  it('shows the saved selection and points to peasant kickstart without a control that edits it', async () => {
    await mountPage();
    const row = rowOf('selection.mode');
    expect(row).toBe(rowOf('selection.harnesses'));
    expect(within(row).getByText('peasant kickstart')).toBeInTheDocument();
    expect(within(row).queryByRole('switch')).toBeNull();
    expect(within(row).queryByRole('combobox')).toBeNull();
    expect(within(row).queryByRole('button', { name: /^edit/ })).toBeNull();
    expect(within(rowOf('selection.autoIngestNewBranches')).queryByRole('switch')).toBeNull();
  });
});

describe('settings page custom patterns', () => {
  for (const testCase of loadPendingPatternWriteCases()) {
    it(testCase.name, async () => {
      const settings = loadSettingsResponse();
      const setting = settings.settings.find(({ key }) => key === 'redaction.custom_patterns')!;
      setting.value = setting.effective = testCase.patterns;
      mountServer({ settings });
      let persisted: unknown = testCase.patterns;
      let release!: () => void;
      serve('PATCH', '/api/v1/settings', (body) => {
        const value = (body as { value: unknown }).value;
        const saved = () => {
          persisted = value;
          return answer(200, { ...setting, value, effective: value });
        };
        if (mutations().length === 1) return new Promise<Answer>((resolve) => { release = () => resolve(saved()); });
        return saved();
      });
      await mountPage();
      const block = rowOf('redaction.custom_patterns');
      const [first, second] = testCase.patterns;
      fireEvent.click(within(block).getByRole('button', { name: `more for ${first.id}` }));
      fireEvent.click(await screen.findByRole('menuitem', { name: 'remove' }));
      await waitFor(() => expect(release).toBeTypeOf('function'));
      expect(within(block).getByRole('button', { name: 'add a pattern' })).toBeDisabled();
      fireEvent.click(within(block).getByRole('button', { name: `more for ${second.id}` }));
      for (const item of await screen.findAllByRole('menuitem')) expect(item).toHaveAttribute('aria-disabled', 'true');
      fireEvent.click(screen.getByRole('menuitem', { name: 'remove' }));
      fireEvent.click(within(block).getByRole('button', { name: 'add a pattern' }));
      expect(mutations().map(({ body }) => body)).toEqual([{ key: setting.key, value: [second] }]);
      expect(persisted).toEqual(testCase.patterns);
      fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
      release();
      await waitFor(() => expect(within(block).queryByText(first.id)).toBeNull());
      expect(persisted).toEqual([second]);
      expect(within(block).getByRole('button', { name: 'add a pattern' })).toBeEnabled();
      fireEvent.click(within(block).getByRole('button', { name: `more for ${second.id}` }));
      expect(await screen.findByRole('menuitem', { name: 'remove' })).not.toHaveAttribute('aria-disabled', 'true');
      fireEvent.click(screen.getByRole('menuitem', { name: 'remove' }));
      await within(block).findByText('no patterns yet. the built-in rules still apply.');
      expect(persisted).toBeNull();
      expect(mutations().map(({ body }) => body)).toEqual([
        { key: setting.key, value: [second] },
        { key: setting.key, value: null },
      ]);
    });
  }

  it('adds a pattern with one write of the whole list, and keeps the list when the write is refused', async () => {
    let refuse = true;
    serve('PATCH', '/api/v1/settings', (body) => {
      if (refuse) return answer(400, { key: 'redaction.custom_patterns', error: 'This value would make the configuration invalid: config: custom pattern "acme-host": invalid regex. Nothing was changed.' });
      const value = (body as { value: unknown }).value;
      return answer(200, { key: 'redaction.custom_patterns', kind: 'structured', value, effective: value, editable: true, inPeasantConfig: false });
    });
    await mountPage();
    const block = rowOf('redaction.custom_patterns');
    fireEvent.click(within(block).getByRole('button', { name: 'add a pattern' }));
    fireEvent.change(within(block).getByLabelText('name'), { target: { value: 'acme-host' } });
    fireEvent.change(within(block).getByLabelText('category'), { target: { value: 'project' } });
    fireEvent.change(within(block).getByLabelText('pattern (a regular expression)'), { target: { value: '[a-z' } });
    fireEvent.change(within(block).getByLabelText('replace with'), { target: { value: '<ACME_HOST>' } });
    fireEvent.click(within(block).getByRole('button', { name: 'save pattern' }));
    expect(await within(rowOf('redaction.custom_patterns')).findByRole('alert')).toHaveTextContent('invalid regex');
    expect(within(rowOf('redaction.custom_patterns')).getByText('no patterns yet. the built-in rules still apply.')).toBeInTheDocument();

    refuse = false;
    fireEvent.change(within(rowOf('redaction.custom_patterns')).getByLabelText('pattern (a regular expression)'), { target: { value: '[a-z]+\\.acme\\.internal' } });
    fireEvent.click(within(rowOf('redaction.custom_patterns')).getByRole('button', { name: 'save pattern' }));
    await within(rowOf('redaction.custom_patterns')).findByText('acme-host');
    expect(mutations().map((call) => call.body)).toEqual([
      { key: 'redaction.custom_patterns', value: [{ id: 'acme-host', category: 'project', pattern: '[a-z', replacement: '<ACME_HOST>' }] },
      { key: 'redaction.custom_patterns', value: [{ id: 'acme-host', category: 'project', pattern: '[a-z]+\\.acme\\.internal', replacement: '<ACME_HOST>' }] },
    ]);
  });
});

describe('settings page load', () => {
  it('shows why the settings could not be read, and reads them again on request', async () => {
    let fail = true;
    serve('GET', '/api/v1/settings', () => (fail
      ? answer(500, { error: 'redaction.level is "maximum", which this version cannot apply. Set redaction.level to standard, then reload.' })
      : answer(200, loadSettingsResponse())));
    render(<SettingsPageClient />);
    expect(await screen.findByText(/redaction.level is "maximum"/)).toBeInTheDocument();
    fail = false;
    fireEvent.click(screen.getByRole('button', { name: 'try again' }));
    await waitFor(() => expect(document.querySelector('details.srow-group')).not.toBeNull());
  });
});

describe('auto-publish install', () => {
  for (const testCase of loadPendingInstallCases()) {
    it(testCase.name, async () => {
      const rule = testCase.rule;
      mountServer({ settings: withRules([rule]), auth: { authenticated: true, username: 'alice-dev' } });
      let release!: (reply: Answer) => void;
      serve('POST', `/api/v1/settings/auto-publish/${rule.id}/install`, () => new Promise<Answer>((resolve) => { release = resolve; }));
      serve('PUT', `/api/v1/settings/auto-publish/${rule.id}`, (body) => answer(200, { ...rule, ...(body as object), repositories: [] }));
      await mountPage();
      const group = document.querySelector<HTMLElement>('[data-group="auto-publish"]')!;
      const item = group.querySelector<HTMLElement>(`[data-rule-id="${rule.id}"]`)!;
      const menu = within(item).getByRole('button', { name: `more for ${rule.match}` });
      const toggle = within(item).getByRole('switch');
      if (testCase.openForm) {
        fireEvent.click(menu);
        fireEvent.click(await screen.findByRole('menuitem', { name: 'edit' }));
        await within(group).findByRole('checkbox', { name: 'Acme Platform' });
        expect(within(group).getByRole('button', { name: 'save rule' })).toBeEnabled();
      }
      fireEvent.click(within(group).getByRole('button', { name: 'install in 1 repository' }));
      await waitFor(() => expect(release).toBeTypeOf('function'));
      expect(toggle).toBeDisabled();

      fireEvent.click(toggle);
      fireEvent.click(menu);
      for (const entry of await screen.findAllByRole('menuitem')) expect(entry).toHaveAttribute('aria-disabled', 'true');
      fireEvent.click(screen.getByRole('menuitem', { name: 'remove' }));
      expect(item).toBeInTheDocument();
      fireEvent.keyDown(menu, { key: 'Escape' });
      if (testCase.openForm) {
        const form = within(group).getByRole('form', { name: `edit ${rule.match}` });
        for (const control of form.querySelectorAll('input,select,button')) expect(control).toBeDisabled();
        fireEvent.click(within(form).getByRole('button', { name: 'save rule' }));
        fireEvent.click(within(form).getByRole('button', { name: 'cancel' }));
        expect(form).toBeInTheDocument();
      } else {
        const add = within(group).getByRole('button', { name: 'add a folder or repository' });
        expect(add).toBeDisabled();
        fireEvent.click(add);
        expect(within(group).queryByRole('form')).toBeNull();
      }
      expect(mutations().map(({ method, path }) => `${method} ${path}`)).toEqual([`POST /api/v1/settings/auto-publish/${rule.id}/install`]);
      const repository = rule.repositories[0];
      release(answer(200, { ...repository, hooks: repository.hooks.map((hook) => ({ ...hook, status: 'installed' })) }));
      await within(group).findByRole('list', { name: 'install results' });
      expect(toggle).toBeEnabled();
      expect(menu).toBeEnabled();
      if (testCase.openForm) {
        const form = within(group).getByRole('form', { name: `edit ${rule.match}` });
        for (const control of form.querySelectorAll('input,select,button')) expect(control).toBeEnabled();
        fireEvent.change(within(form).getByLabelText('folder'), { target: { value: testCase.nextMatch } });
        fireEvent.click(within(form).getByRole('button', { name: 'save rule' }));
        await waitFor(() => expect(group.querySelector('[data-rule-id]')?.textContent).toContain(testCase.nextMatch));
      } else {
        expect(within(group).getByRole('button', { name: 'add a folder or repository' })).toBeEnabled();
        fireEvent.click(toggle);
        await within(group).findByText(/paused, publishes nothing/);
      }
      expect(mutations().map(({ method, path }) => `${method} ${path}`)).toEqual([
        `POST /api/v1/settings/auto-publish/${rule.id}/install`,
        `PUT /api/v1/settings/auto-publish/${rule.id}`,
      ]);
      expect((mutations()[1].body as { events: string[]; match: string })).toMatchObject(testCase.openForm
        ? { events: rule.events, match: testCase.nextMatch }
        : { events: [], match: rule.match });
    });
  }
  it.each(loadInstallCases().map((testCase) => [testCase.name, testCase] as const))('%s', async (_name, testCase) => {
    mountServer({ settings: withRules(testCase.rules), auth: { authenticated: true, username: 'alice-dev' } });
    serve('POST', '/api/v1/settings/auto-publish/acme-work/install', (body) => testCase.answers[(body as { path: string }).path]);
    serve('POST', '/api/v1/settings/auto-publish/acme-remote/install', (body) => testCase.answers[(body as { path: string }).path]);
    await mountPage();
    const group = document.querySelector<HTMLElement>('[data-group="auto-publish"]')!;
    const installs = () => mutations().filter((call) => call.path.endsWith('/install'));

    if (testCase.offer === null) {
      expect(within(group).queryByRole('button', { name: /^install in/ })).toBeNull();
      expect(mutations()).toEqual([]);
      return;
    }
    const action = within(group).getByRole('button', { name: testCase.offer });
    // Nothing is installed before the click, and every repository discloses what
    // the install will do first.
    expect(mutations()).toEqual([]);
    const pending = within(group).getByRole('list', { name: 'repositories to install in' });
    for (const [path, texts] of Object.entries(testCase.preview)) {
      const item = Array.from(pending.querySelectorAll('li')).find((li) => li.querySelector(`[title="${path}"]`));
      expect(item, `the preview for ${path}`).toBeDefined();
      const details = item!.querySelector<HTMLDetailsElement>('details.stg-install-preview');
      expect(details, `${path} has an install preview`).not.toBeNull();
      details!.open = true;
      for (const text of texts) expect(item!.textContent, `${path} preview shows ${text}`).toContain(text);
    }
    fireEvent.click(action);
    const results = await within(group).findByRole('list', { name: 'install results' });
    expect(installs().map((call) => `${call.path.split('/').at(-2)} -> ${(call.body as { path: string }).path}`)).toEqual(testCase.calls);
    for (const [path, texts] of Object.entries(testCase.shows)) {
      const item = Array.from(results.querySelectorAll('li')).find((li) => li.querySelector(`[title="${path}"]`));
      expect(item, `the result for ${path}`).toBeDefined();
      for (const text of texts) expect(item!.textContent, `${path} shows ${text}`).toContain(text);
    }
    for (const [ruleId, texts] of Object.entries(testCase.refreshed)) {
      const item = group.querySelector<HTMLElement>(`[data-rule-id="${ruleId}"]`)!;
      for (const text of texts) expect(item.textContent, `${ruleId} shows ${text}`).toContain(text);
    }
  });
});

describe('auto-publish rules', () => {
  for (const testCase of loadPendingRuleWriteCases()) {
    it(testCase.name, async () => {
      const [first, second] = testCase.rules;
      mountServer({ settings: withRules(testCase.rules) });
      let release!: (reply: Answer) => void;
      serve('PUT', `/api/v1/settings/auto-publish/${first.id}`, () => new Promise<Answer>((resolve) => { release = resolve; }));
      serve('PUT', `/api/v1/settings/auto-publish/${second.id}`, (body) => answer(200, { ...second, ...(body as object), repositories: [] }));
      await mountPage();
      const group = document.querySelector<HTMLElement>('[data-group="auto-publish"]')!;
      const ruleItem = (id: string) => group.querySelector<HTMLElement>(`[data-rule-id="${id}"]`)!;
      const firstToggle = within(ruleItem(first.id)).getByRole('switch');
      const secondToggle = within(ruleItem(second.id)).getByRole('switch');
      fireEvent.click(firstToggle);
      await waitFor(() => expect(release).toBeTypeOf('function'));
      expect(firstToggle).toBeDisabled();
      expect(secondToggle).toBeDisabled();
      const install = within(group).getByRole('button', { name: 'install in 2 repositories' });
      expect(install).toBeDisabled();
      fireEvent.click(secondToggle);
      fireEvent.click(install);
      fireEvent.click(within(ruleItem(second.id)).getByRole('button', { name: `more for ${second.match}` }));
      for (const entry of await screen.findAllByRole('menuitem')) expect(entry).toHaveAttribute('aria-disabled', 'true');
      fireEvent.click(screen.getByRole('menuitem', { name: 'remove' }));
      fireEvent.keyDown(screen.getByRole('menuitem', { name: 'remove' }), { key: 'Escape' });
      expect(mutations().map(({ method, path }) => `${method} ${path}`)).toEqual([`PUT /api/v1/settings/auto-publish/${first.id}`]);
      release(answer(200, { ...first, events: [], repositories: [] }));
      await within(ruleItem(first.id)).findByText(/paused, publishes nothing/);
      expect(within(ruleItem(second.id)).getByRole('switch')).toHaveAttribute('aria-checked', 'true');
      expect(secondToggle).toBeEnabled();
      expect(within(group).getByRole('button', { name: 'install in 1 repository' })).toBeEnabled();
      fireEvent.click(secondToggle);
      await within(ruleItem(second.id)).findByText(/paused, publishes nothing/);
      expect(within(ruleItem(first.id)).getByRole('switch')).toHaveAttribute('aria-checked', 'false');
      expect(mutations().map(({ method, path, body }) => ({ method, path, events: (body as { events: string[] }).events }))).toEqual([
        { method: 'PUT', path: `/api/v1/settings/auto-publish/${first.id}`, events: [] },
        { method: 'PUT', path: `/api/v1/settings/auto-publish/${second.id}`, events: [] },
      ]);
    });
  }
  const rule: AutoPublishRule = {
    id: 'acme-work',
    kind: 'folder',
    match: '~/work/acme/**',
    events: ['pre-push'],
    collectives: ['3f9c1a2b-0000-4000-8000-000000000001'],
    repositories: [{ path: '/home/alice/work/acme/api', label: 'github.com:acme/api', hooks: [{ event: 'pre-push', status: 'installed' }] }],
  };

  it('names the collectives when signed in, and pauses a rule with one save and no install', async () => {
    mountServer({ settings: withRules([rule]), auth: { authenticated: true, username: 'alice-dev' } });
    serve('PUT', '/api/v1/settings/auto-publish/acme-work', (body) => answer(200, { ...rule, ...(body as object), repositories: [{ ...rule.repositories[0], hooks: [] }] }));
    await mountPage();
    const item = document.querySelector<HTMLElement>('[data-rule-id="acme-work"]')!;
    await within(item).findByText(/publishes to Acme Platform on git push/);
    fireEvent.click(within(item).getByRole('switch'));
    await within(document.querySelector<HTMLElement>('[data-rule-id="acme-work"]')!).findByText(/paused, publishes nothing/);
    expect(mutations().map(({ method, path, body }) => ({ method, path, body }))).toEqual([
      { method: 'PUT', path: '/api/v1/settings/auto-publish/acme-work', body: { kind: 'folder', match: '~/work/acme/**', events: [], collectives: ['3f9c1a2b-0000-4000-8000-000000000001'] } },
    ]);
  });

  it('editing only the collectives keeps both of a two-event rule\'s events', async () => {
    const twoEvent: AutoPublishRule = { ...rule, events: ['pre-push', 'post-commit'], repositories: [] };
    mountServer({ settings: withRules([twoEvent]), auth: { authenticated: true, username: 'alice-dev' } });
    serve('PUT', '/api/v1/settings/auto-publish/acme-work', (body) => answer(200, { ...twoEvent, ...(body as object), repositories: [] }));
    await mountPage();
    const group = document.querySelector<HTMLElement>('[data-group="auto-publish"]')!;
    fireEvent.click(within(group).getByRole('button', { name: 'more for ~/work/acme/**' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: 'edit' }));
    const form = within(group).getByRole('form', { name: 'edit ~/work/acme/**' });
    // Both events start checked from the rule, not from a default.
    expect(within(form).getByRole('checkbox', { name: 'on git push' })).toBeChecked();
    expect(within(form).getByRole('checkbox', { name: 'on each commit' })).toBeChecked();
    fireEvent.click(within(form).getByRole('checkbox', { name: 'Acme Labs' }));
    fireEvent.click(within(form).getByRole('button', { name: 'save rule' }));
    await waitFor(() => expect(mutations()).toHaveLength(1));
    expect(mutations()[0].body).toEqual({
      kind: 'folder',
      match: '~/work/acme/**',
      events: ['pre-push', 'post-commit'],
      collectives: ['3f9c1a2b-0000-4000-8000-000000000001', '3f9c1a2b-0000-4000-8000-000000000002'],
    });
  });

  it('a deliberate event toggle saves only the chosen events', async () => {
    const twoEvent: AutoPublishRule = { ...rule, events: ['pre-push', 'post-commit'], repositories: [] };
    mountServer({ settings: withRules([twoEvent]), auth: { authenticated: true, username: 'alice-dev' } });
    serve('PUT', '/api/v1/settings/auto-publish/acme-work', (body) => answer(200, { ...twoEvent, ...(body as object), repositories: [] }));
    await mountPage();
    const group = document.querySelector<HTMLElement>('[data-group="auto-publish"]')!;
    fireEvent.click(within(group).getByRole('button', { name: 'more for ~/work/acme/**' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: 'edit' }));
    const form = within(group).getByRole('form', { name: 'edit ~/work/acme/**' });
    fireEvent.click(within(form).getByRole('checkbox', { name: 'on each commit' }));
    fireEvent.click(within(form).getByRole('button', { name: 'save rule' }));
    await waitFor(() => expect(mutations()).toHaveLength(1));
    expect((mutations()[0].body as { events: string[] }).events).toEqual(['pre-push']);
  });

  it('adds a rule with one save, installs nothing, then offers the repositories it covers', async () => {
    mountServer({ settings: withRules([]), auth: { authenticated: true, username: 'alice-dev' } });
    serve('PUT', '/api/v1/settings/auto-publish/*', (body, path) => answer(200, {
      id: decodeURIComponent(path.split('/').at(-1)!),
      ...(body as object),
      repositories: [{ path: '/home/alice/work/acme/api', label: 'github.com:acme/api', hooks: [{ event: 'pre-push', status: 'absent' }] }],
    }));
    await mountPage();
    const group = document.querySelector<HTMLElement>('[data-group="auto-publish"]')!;
    fireEvent.click(within(group).getByRole('button', { name: 'add a folder or repository' }));
    fireEvent.change(within(group).getByLabelText('folder'), { target: { value: '~/work/acme/**' } });
    fireEvent.click(await within(group).findByRole('checkbox', { name: 'Acme Platform' }));
    fireEvent.click(within(group).getByRole('button', { name: 'save rule' }));
    await within(group).findByRole('button', { name: 'install in 1 repository' });
    const saves = mutations();
    expect(saves).toHaveLength(1);
    expect(saves[0].method).toBe('PUT');
    expect(saves[0].path).toMatch(/^\/api\/v1\/settings\/auto-publish\/rule-[0-9a-f]{12}$/);
    expect(saves[0].body).toEqual({ kind: 'folder', match: '~/work/acme/**', events: ['pre-push'], collectives: ['3f9c1a2b-0000-4000-8000-000000000001'] });
  });

  it('removes a rule and explains that retained hooks require another active binding', async () => {
    mountServer({ settings: withRules([rule]) });
    serve('DELETE', '/api/v1/settings/auto-publish/acme-work', () => answer(200, { id: 'acme-work', repositories: rule.repositories }));
    await mountPage();
    const item = document.querySelector<HTMLElement>('[data-rule-id="acme-work"]')!;
    fireEvent.click(within(item).getByRole('button', { name: 'more for ~/work/acme/**' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: 'remove' }));
    expect(await screen.findByRole('status')).toHaveTextContent('its hook stays in 1 repository as files. rule-required hooks publish only if another active binding covers the repository and event. separately installed terminal hooks keep their own consent.');
    expect(document.querySelector('[data-rule-id="acme-work"]')).toBeNull();
    expect(mutations().map(({ method, path }) => `${method} ${path}`)).toEqual(['DELETE /api/v1/settings/auto-publish/acme-work']);
  });
});
