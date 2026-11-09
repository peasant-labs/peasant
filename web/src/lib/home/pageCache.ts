import { zLocalSyncSessionPage, type LocalSyncSessionPage } from '@peasant-labs/schema';
import type { HomeCacheKey } from './viewState';

/**
 * The Home tab cache: one `LocalSyncSessionPage` per key in `sessionStorage`.
 * The key carries the account and the view scope, the page size, and the page
 * number. The value carries the server `revision`, so a revision mismatch can
 * clear it. The cache holds list rows only, never transcript bodies, and its
 * lifetime is the tab.
 */
export interface HomePageCache {
  read(key: HomeCacheKey): LocalSyncSessionPage | null;
  write(key: HomeCacheKey, page: LocalSyncSessionPage): void;
  clear(): void;
}

const CACHE_PREFIX = 'peasant.home.page:';

type CacheStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem' | 'key' | 'length'>;

function defaultStorage(): CacheStorage | null {
  return typeof window === 'undefined' ? null : window.sessionStorage;
}

function pageKeys(storage: CacheStorage): string[] {
  const keys: string[] = [];
  for (let index = 0; index < storage.length; index++) {
    const key = storage.key(index);
    if (key !== null && key.startsWith(CACHE_PREFIX)) keys.push(key);
  }
  return keys;
}

/**
 * Build the cache over a storage object. Tests pass an in-memory storage; the
 * app uses `sessionStorage`. A storage that throws (private mode, quota) reads
 * as empty and writes nothing.
 */
export function createHomePageCache(storage: CacheStorage | null = defaultStorage()): HomePageCache {
  const read = (key: HomeCacheKey): LocalSyncSessionPage | null => {
    if (storage === null) return null;
    let raw: string | null;
    try {
      raw = storage.getItem(CACHE_PREFIX + key);
    } catch {
      return null;
    }
    if (raw === null) return null;
    try {
      const parsed = zLocalSyncSessionPage.safeParse(JSON.parse(raw));
      return parsed.success ? parsed.data : null;
    } catch {
      return null;
    }
  };
  return {
    read,
    write(key, page) {
      if (storage === null) return;
      try {
        storage.setItem(CACHE_PREFIX + key, JSON.stringify(page));
      } catch {
        // A full quota drops the cache rather than failing the page.
      }
    },
    clear() {
      if (storage === null) return;
      for (const key of pageKeys(storage)) {
        try {
          storage.removeItem(key);
        } catch {
          // Ignore a storage that refuses removal; the next write overwrites it.
        }
      }
    },
  };
}
