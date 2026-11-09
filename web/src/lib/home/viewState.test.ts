import { describe, expect, it } from 'vitest';
import { PageSize, newProjectHash } from '@peasant-labs/schema';
import { HomeGroupBy } from '@/components/home/homeColumns';
import {
  ALL_BROWSE_VIEWS,
  BrowseView,
  decodeHomeViewState,
  encodeHomeViewState,
  homeCacheKey,
  parseBrowseView,
  type HomeViewState,
} from './viewState';
import { clearHomeOpenGroups, readHomeDeviceView, writeHomeDeviceView } from './deviceView';

const PROJECT = 'a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90';

const FULL: HomeViewState = {
  browse: BrowseView.Projects,
  publication: 'published',
  project: newProjectHash(PROJECT),
  query: 'flaky test',
  pageSize: 50,
  page: 3,
  groupBy: HomeGroupBy.None,
};

describe('BrowseView', () => {
  it('ALL_BROWSE_VIEWS accounts for every member exactly once', () => {
    expect(ALL_BROWSE_VIEWS.map(member => member.wire).sort()).toEqual(['flat', 'grouped', 'projects', 'sessions']);
    expect(new Set(ALL_BROWSE_VIEWS).size).toBe(ALL_BROWSE_VIEWS.length);
    // Every declared member is reachable from the list, so a new member fails
    // the MissingBrowseView assertion in viewState.ts at compile time.
    for (const member of ALL_BROWSE_VIEWS) expect(parseBrowseView(member.wire)).toBe(member);
  });

  it('an unknown or absent value takes the declared default', () => {
    expect(parseBrowseView('not-a-view')).toBe(BrowseView.Sessions);
    expect(parseBrowseView(undefined)).toBe(BrowseView.Sessions);
    expect(parseBrowseView(null)).toBe(BrowseView.Sessions);
  });

  it('labels the four views as the menu does', () => {
    expect(ALL_BROWSE_VIEWS.map(member => member.label)).toEqual(['sessions', 'project', 'helper threads', 'every session']);
  });
});

describe('home view codec', () => {
  it('an absent URL takes the declared defaults', () => {
    expect(decodeHomeViewState('')).toEqual({
      browse: BrowseView.Sessions,
      publication: 'all',
      project: null,
      query: '',
      pageSize: PageSize.P25,
      page: 1,
      groupBy: HomeGroupBy.Project,
    });
  });

  it('an unknown value takes the declared default', () => {
    const view = decodeHomeViewState('?browse=nope&publication=nope&perPage=26&page=0&groupBy=nope&project=short');
    expect(view.browse).toBe(BrowseView.Sessions);
    expect(view.publication).toBe('all');
    expect(view.pageSize).toBe(PageSize.P25);
    expect(view.page).toBe(1);
    expect(view.groupBy).toBe(HomeGroupBy.Project);
    expect(view.project).toBeNull();
  });

  it('round-trips a full view through the URL', () => {
    const search = encodeHomeViewState(FULL);
    const decoded = decodeHomeViewState(search);
    expect(decoded.browse).toBe(FULL.browse);
    expect(decoded.publication).toBe(FULL.publication);
    expect(decoded.project).toBe(FULL.project);
    expect(decoded.query).toBe(FULL.query);
    expect(decoded.pageSize).toBe(FULL.pageSize);
    expect(decoded.page).toBe(FULL.page);
    expect(decoded.groupBy).toBe(FULL.groupBy);
  });

  it('omits every default from the encoded URL', () => {
    expect(encodeHomeViewState(decodeHomeViewState(''))).toBe('');
  });

  it('carries the grouping but never the device columns or open groups', () => {
    const search = encodeHomeViewState(FULL);
    expect(search).toContain('groupBy=none');
    expect(search).not.toContain('visibleColumns');
    expect(search).not.toContain('open=');
  });
});

describe('homeCacheKey', () => {
  it('includes the account, scope, size, and page', () => {
    expect(homeCacheKey(FULL, 'first')).toContain('first');
    expect(homeCacheKey(FULL, 'first')).toContain('projects');
    expect(homeCacheKey(FULL, 'first')).toContain('published');
    expect(homeCacheKey(FULL, 'first')).toContain(PROJECT);
    expect(homeCacheKey(FULL, 'first')).toContain('50');
  });

  it('omits the grouping and the revision: grouping is layout, not rows', () => {
    const other = { ...FULL, groupBy: HomeGroupBy.Project };
    expect(homeCacheKey(FULL, 'first')).toBe(homeCacheKey(other, 'first'));
  });

  it('distinguishes the page and the account', () => {
    expect(homeCacheKey(FULL, 'first')).not.toBe(homeCacheKey({ ...FULL, page: 4 }, 'first'));
    expect(homeCacheKey(FULL, 'first')).not.toBe(homeCacheKey(FULL, 'second'));
  });
});

describe('device view storage', () => {
  const storage = () => {
    const map = new Map<string, string>();
    return {
      getItem: (key: string) => map.get(key) ?? null,
      setItem: (key: string, value: string) => { map.set(key, value); },
    };
  };

  it('reads the default when nothing is stored', () => {
    expect(readHomeDeviceView(storage())).toEqual({ visibleColumns: null, open: null });
  });

  it('round-trips the columns and open groups', () => {
    const store = storage();
    writeHomeDeviceView({ visibleColumns: ['session', 'state'], open: ['project:alpha'] }, store);
    expect(readHomeDeviceView(store)).toEqual({ visibleColumns: ['session', 'state'], open: ['project:alpha'] });
  });

  it('a group-by change clears the open set and keeps the columns', () => {
    const store = storage();
    writeHomeDeviceView({ visibleColumns: ['session'], open: ['project:alpha'] }, store);
    clearHomeOpenGroups(store);
    expect(readHomeDeviceView(store)).toEqual({ visibleColumns: ['session'], open: null });
  });

  it('discards a value that is not the shape this app writes', () => {
    const store = storage();
    store.setItem('peasant.home', JSON.stringify({ visibleColumns: ['not-a-column'], open: 'nope' }));
    expect(readHomeDeviceView(store)).toEqual({ visibleColumns: null, open: null });
  });
});
