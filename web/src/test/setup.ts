import '@testing-library/jest-dom/vitest';

// jsdom's bundled `localStorage` is a non-functional stub (no get/set/remove)
// unless the environment is configured with a URL + storage quota. Install a
// small in-memory Storage implementation so hooks that persist to
// localStorage (e.g. useTheme, useTourState) work under test.
class MemoryStorage implements Storage {
  private store = new Map<string, string>();

  get length(): number {
    return this.store.size;
  }

  clear(): void {
    this.store.clear();
  }

  getItem(key: string): string | null {
    return this.store.has(key) ? (this.store.get(key) as string) : null;
  }

  key(index: number): string | null {
    return Array.from(this.store.keys())[index] ?? null;
  }

  removeItem(key: string): void {
    this.store.delete(key);
  }

  setItem(key: string, value: string): void {
    this.store.set(key, String(value));
  }
}

const memoryStorage = new MemoryStorage();
Object.defineProperty(window, 'localStorage', {
  configurable: true,
  value: memoryStorage,
});

// jsdom has no layout engine, so Element.scrollTo / window.scrollTo are unimplemented.
// Components that smooth-scroll (e.g. the lifted ChangeDetail's proof-jump) otherwise throw
// "scrollTo is not a function" as an UNHANDLED error under test (vitest reports it as an
// error even when the assertion passes). Install no-op stubs so the real handler runs.
const noopScroll = () => undefined;
if (typeof Element.prototype.scrollTo !== 'function') {
  Element.prototype.scrollTo = noopScroll as unknown as typeof Element.prototype.scrollTo;
}
if (typeof window.scrollTo !== 'function') {
  window.scrollTo = noopScroll as unknown as typeof window.scrollTo;
}
