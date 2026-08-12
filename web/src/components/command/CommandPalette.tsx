'use client';

import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { useRouter } from 'next/navigation';
import { SearchIcon } from 'lucide-react';
import { useTheme } from '@/hooks/useTheme';
import { NAV_SECTIONS } from '@/lib/nav/sections';
import { fetchProjectSummaries, fetchSearch } from '@/lib/api/map';
import { fetchDiscovery, requireDiscoveryItem, type DiscoveryItem } from '@/lib/api/discovery';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import { displayProject } from '@/lib/quality/utils';
import { mapHref, parseProjectHash, reviewHref, transcriptHref } from '@/lib/navigation/projectRoutes';
import type { ProjectSummary, SearchResult } from '@peasant-labs/schema';

/**
 * Cmd/Ctrl-K command palette: jump to a project (its changes or its
 * map), a nav section, or run a quick action (toggle theme). Self-contained,
 * mounted once globally in the layout shell. Strict-monochrome, radius-0.
 *
 * The IA nav module (NAV_SECTIONS) and the project-summary endpoint already
 * exist; this surfaces them behind one keystroke.
 *
 * The server-side "Messages" group debounces queries of at least two characters,
 * full-text-searches recorded transcripts via /api/v1/search, and deep-links each
 * hit to its task turn. These results are server-ranked, so —
 * unlike the local commands — they are NOT run through filterCommands; they are
 * appended after the filtered local commands so keyboard nav spans the whole list.
 */

export interface Command {
  id: string;
  label: string;
  /** Right-aligned group label (e.g. "Project", "Go to", "Action"). */
  group: string;
  /** Extra text folded into the match (e.g. the raw project path). */
  keywords?: string;
  searchAnnotation?: SearchAnnotation;
  run: () => void;
}

export interface SearchAnnotation {
  discovery: DiscoveryItem;
}

export interface AnnotatedSearchResult extends SearchResult, SearchAnnotation {}

export function annotateSearchResults(
  results: SearchResult[],
  discovery: ReadonlyMap<string, DiscoveryItem>,
): AnnotatedSearchResult[] {
  return results.map((result) => ({
    ...result,
    discovery: requireDiscoveryItem(discovery, result.sessionId, 'command palette search'),
  }));
}

/** Case-insensitive subsequence-free substring filter over label + keywords. */
export function filterCommands(commands: Command[], query: string): Command[] {
  const q = query.trim().toLowerCase();
  if (!q) return commands;
  return commands.filter(
    (c) =>
      c.label.toLowerCase().includes(q) ||
      c.group.toLowerCase().includes(q) ||
      (c.keywords ?? '').toLowerCase().includes(q),
  );
}

/** Debounce before firing a transcript search as the user types (ms). */
const SEARCH_DEBOUNCE_MS = 180;
/** Minimum query length before searching (mirrors the server's 2-char floor). */
const SEARCH_MIN_CHARS = 2;

/** Collapse a FTS5 snippet to a single trimmed line for the command label. */
export function messageLabel(snippet: string): string {
  return snippet.replace(/\s+/g, ' ').trim();
}

/** Event name a visible affordance (e.g. the nav ⌘K pill) dispatches to open
 *  the palette without synthesizing a keyboard event. */
export const OPEN_COMMAND_PALETTE_EVENT = 'peasant:open-command-palette';

/** Global Cmd/Ctrl-K toggle (also opens on the OPEN_COMMAND_PALETTE_EVENT). */
export function useCommandPaletteHotkey(): {
  open: boolean;
  setOpen: (open: boolean) => void;
} {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setOpen((o) => !o);
      }
    };
    const onOpen = () => setOpen(true);
    document.addEventListener('keydown', onKey);
    window.addEventListener(OPEN_COMMAND_PALETTE_EVENT, onOpen);
    return () => {
      document.removeEventListener('keydown', onKey);
      window.removeEventListener(OPEN_COMMAND_PALETTE_EVENT, onOpen);
    };
  }, []);
  return { open, setOpen };
}

export function CommandPalette() {
  const { open, setOpen } = useCommandPaletteHotkey();
  const router = useRouter();
  const { toggle: toggleTheme } = useTheme();

  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const [projects, setProjects] = useState<ProjectSummary[] | null>(null);
  const [projectError, setProjectError] = useState<unknown>(null);
  const [projectReload, setProjectReload] = useState(0);
  const [messages, setMessages] = useState<AnnotatedSearchResult[]>([]);
  const [searchError, setSearchError] = useState<unknown>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();

  const close = useCallback(() => {
    setOpen(false);
    setQuery('');
    setActiveIndex(0);
    setMessages([]);
    setSearchError(null);
  }, [setOpen]);

  const go = useCallback(
    (href: string) => () => {
      close();
      router.push(href);
    },
    [close, router],
  );

  // Fetch projects on first open (cheap, cached for the session).
  useEffect(() => {
    if (open && projects === null && projectError === null) {
      setProjectError(null);
      fetchProjectSummaries()
        .then((payload) => setProjects(payload.projects))
        .catch((error: unknown) => {
          setProjects(null);
          setProjectError(error);
        });
    }
  }, [open, projects, projectError, projectReload]);

  // Debounced transcript search. A cancelled flag drops late responses so a
  // slow request for a stale query can't overwrite a newer result set.
  useEffect(() => {
    const q = query.trim();
    if (!open || q.length < SEARCH_MIN_CHARS) {
      setMessages([]);
      return;
    }
    let cancelled = false;
    const handle = setTimeout(() => {
      Promise.all([fetchSearch(q, 20), fetchDiscovery()])
        .then(([search, discovery]) => {
          if (!cancelled) {
            setMessages(annotateSearchResults(search.results, discovery));
            setSearchError(null);
          }
        })
        .catch((error: unknown) => {
          if (!cancelled) {
            setMessages([]);
            setSearchError(error);
          }
        });
    }, SEARCH_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(handle);
    };
  }, [query, open]);

  // Focus the input when opened; reset highlight as the query changes.
  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);
  useEffect(() => {
    setActiveIndex(0);
  }, [query]);

  const commands = useMemo<Command[]>(() => {
    const navCmds: Command[] = NAV_SECTIONS.map((s) => ({
      id: `nav:${s.href}`,
      label: `go to ${s.label}`,
      group: 'Go to',
      run: go(s.href),
    }));
    const actionCmds: Command[] = [
      {
        id: 'action:theme',
        label: 'toggle light / dark theme',
        group: 'Action',
        keywords: 'dark light mode appearance',
        run: () => {
          close();
          toggleTheme();
        },
      },
    ];
    const projectCmds: Command[] = (projects ?? []).flatMap((p) => {
      const projectHash = parseProjectHash(p.projectHash);
      if (!projectHash) return [];
      const name = displayProject(p.project);
      return [
        {
          id: `proj-changes:${p.projectHash}`,
          label: `${name} · changes`,
          group: 'Project',
          keywords: p.project,
          run: go(reviewHref(projectHash)),
        },
        {
          id: `proj-map:${p.projectHash}`,
          label: `${name} · map`,
          group: 'Project',
          keywords: p.project,
          run: go(mapHref(projectHash)),
        },
      ];
    });
    return [...projectCmds, ...navCmds, ...actionCmds];
  }, [projects, go, toggleTheme, close]);

  // Server-ranked transcript hits — deep-link each to its task turn. Kept OUT
  // of filterCommands (the snippet may not contain the literal query) and
  // appended after the filtered local commands.
  const messageCmds = useMemo<Command[]>(
    () =>
      messages.flatMap((r) => {
        const projectHash = parseProjectHash(r.projectHash);
        if (!projectHash) return [];
         return [{
         id: `msg:${r.sessionId}:${r.entryIndex}`,
         label: messageLabel(r.snippet),
         group: 'Messages',
         keywords: r.project,
         searchAnnotation: { discovery: r.discovery },
         run: go(transcriptHref(projectHash, r.sessionId, { turn: r.entryIndex })),
      }];
      }),
    [messages, go],
  );

  const results = useMemo(
    () => [...filterCommands(commands, query), ...messageCmds],
    [commands, query, messageCmds],
  );

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActiveIndex((i) => Math.min(i + 1, Math.max(results.length - 1, 0)));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActiveIndex((i) => Math.max(i - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      (results[activeIndex] ?? results[0])?.run();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      close();
    }
  };

  if (!open || typeof document === 'undefined') return null;

  return createPortal(
    <div
      className="fixed inset-0 z-[120] flex items-start justify-center px-4 pt-[12vh] animate-fade-in"
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
    >
      {/* Scrim — click to dismiss.
          DESIGN_SYSTEM: bg-black/55 is the sanctioned modal-backdrop exception
          to the no-opacity-surface rule (same scrim the TourOverlay uses). */}
      <button
        type="button"
        aria-label="Close command palette"
        className="absolute inset-0 bg-black/55 cursor-default"
        onClick={close}
      />

      <div className="relative w-full max-w-xl border border-rule bg-surface">
        <div className="flex items-center gap-2 border-b border-rule px-3">
          <SearchIcon size={14} className="shrink-0 text-ink-3" aria-hidden />
          <input
            ref={inputRef}
            type="text"
            role="combobox"
            aria-expanded
            aria-controls={listId}
            aria-activedescendant={
              results[activeIndex] ? `${listId}-${activeIndex}` : undefined
            }
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder="jump to a project, page, or action…"
            className="w-full bg-transparent py-3 text-sm text-ink placeholder:text-ink-4 focus:outline-none"
          />
          <kbd className="shrink-0 border border-rule px-1.5 py-0.5 font-mono text-[10px] text-ink-4">
            esc
          </kbd>
        </div>

        <ul id={listId} role="listbox" className="max-h-[50vh] overflow-y-auto py-1">
           {(projectError !== null || searchError !== null) && (
             <li role="alert" className="px-3 py-3 text-base leading-relaxed text-danger">
               <p>{discoveryErrorMessage(projectError ?? searchError)}</p>
               <button
                type="button"
                className="mt-3 border border-rule px-3 py-2 font-mono text-sm text-ink focus-mono"
                 onClick={() => {
                   if (projectError !== null) {
                     setProjectError(null);
                     setProjectReload((value) => value + 1);
                   } else {
                     setSearchError(null);
                     setQuery((value) => `${value} `);
                   }
                 }}
               >
                 {projectError !== null ? 'retry project discovery' : 'retry search discovery'}
              </button>
            </li>
          )}
           {!projectError && !searchError && results.length === 0 ? (
            <li className="px-3 py-6 text-center text-[13px] text-ink-3">
              no matches{projects === null ? ' · loading…' : '.'}
            </li>
           ) : !projectError && !searchError ? (
            results.map((c, i) => (
              <li
                key={c.id}
                id={`${listId}-${i}`}
                role="option"
                aria-selected={i === activeIndex}
              >
                 <button
                  type="button"
                  // Pointer hover mirrors keyboard highlight; mousedown (not
                  // click) so the input's blur doesn't beat the navigation.
                  onMouseEnter={() => setActiveIndex(i)}
                  onMouseDown={(e) => {
                    e.preventDefault();
                    c.run();
                  }}
                  className={`flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-[13px] focus-mono cursor-pointer ${
                    i === activeIndex ? 'bg-surface-hover text-ink' : 'text-ink-2'
                  }`}
                >
                   <span className="min-w-0 truncate">
                     {c.searchAnnotation ? (
                       <>
                         <span>{c.label}</span>
                         <span className="ml-2 text-ink-3">
                           {c.searchAnnotation.discovery.locationLabel}
                           {' · '}{c.searchAnnotation.discovery.branch || 'no branch'}
                           {' · '}{c.searchAnnotation.discovery.selectionStatus}
                         </span>
                       </>
                     ) : c.label}
                   </span>
                  <span className="v2-eyebrow shrink-0">{c.group}</span>
                </button>
              </li>
            ))
          ) : null}
        </ul>
      </div>
    </div>,
    document.body,
  );
}
