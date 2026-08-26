/**
 * Pure utility functions for quality analytics.
 */

// ---------------------------------------------------------------------------
// Project path decoding & display
// ---------------------------------------------------------------------------

/**
 * Common ancestor directory names that are never the project itself. Used when
 * recovering a project name from an encoded path.
 */
const ANCESTOR_DIRS = new Set([
  "Desktop",
  "Documents",
  "Projects",
  "Developer",
  "src",
  "code",
  "repos",
  "workspace",
  "dev",
]);

/**
 * Detect whether a string is a Claude-encoded project path — an *absolute*
 * filesystem path with separators turned into dashes, e.g.
 * `-Users-sampleuser-Desktop-widget-demo` (Claude Code stores
 * `~/.claude/projects/<encoded>`).
 *
 * The encoded form always begins with the encoded leading `/`, i.e. a leading
 * dash. This is what distinguishes it from a plain dash-separated project name
 * such as `sample-project` (no leading dash → not a path).
 */
function isClaudeEncodedPath(s: string): boolean {
  return /^-[A-Za-z]/.test(s) && !s.includes("/") && !s.includes("\\");
}

/**
 * Decode a raw project identifier into a real, human-readable filesystem path.
 *
 * This is the canonical decoding step — it must run BEFORE any truncation so a
 * Claude-encoded blob is never truncated as an opaque string. Handles:
 *
 *  - Claude-encoded paths:  `-Users-sampleuser-Desktop-widget-demo` → `/Users/sampleuser/Desktop/widget/demo`
 *  - Host slugs:            `~Users-acme-dev-Documents-Projects-phaze` → `~/Users/acme/dev/Documents/Projects/phaze`
 *  - Real paths:            passed through unchanged (POSIX or Windows).
 *  - Bare project names:    passed through unchanged.
 *
 * Returns the decoded path. The clean project name is `displayProject()`.
 */
export function decodeProjectPath(project: string): string {
  if (!project) return "";

  // Host slugs look like "~Users-acme-dev-Documents-Projects-phaze": a leading
  // "~" then dash-joined segments. Decode to "~/Users/acme/.../phaze".
  if (project.startsWith("~")) {
    const segments = project.slice(1).split("-").filter(Boolean);
    if (segments.length > 0) return "~/" + segments.join("/");
    return project;
  }

  // Claude-encoded path: dash-joined absolute path. Recover the slash form.
  // The encoded form is the absolute path, so it always starts at root "/".
  if (isClaudeEncodedPath(project)) {
    const parts = project.split("-").filter(Boolean);
    if (parts.length > 0) return "/" + parts.join("/");
    return project;
  }

  // Already a real filesystem path (or a bare name) — pass through.
  return project;
}

/**
 * A server-computed "host:owner/repo" display label (e.g. "github.com:acme/widgets",
 * "gitlab.com:acme/widgets") — the git-remote-derived project name peasant now
 * prefers over a raw path. Detected up front so
 * `displayProject`'s path/basename recovery below never mangles it: the label
 * legitimately contains a "/" (between owner and repo), which would otherwise
 * get truncated down to just the repo name by the path-segment logic.
 *
 * DESIGN NOTE (reviewed): this distinguishes a server-formatted label from a
 * raw path/hash by shape-sniffing the SAME overloaded `project` wire field,
 * rather than carrying an explicit peasant-local boolean alongside it (the
 * way SelectionState is its own typed, unambiguous field rather than folded
 * into an existing one). That's a deliberate, narrower choice here: `project`
 * is a pure display string with no security or access-control meaning (unlike
 * selection scoping), so the cost of a wrong classification is cosmetic
 * (worst case: an oddly-shaped local project name is shown as-is instead of
 * basename-reduced, or vice versa) — not a data leak or a crash. Threading an
 * explicit "isRemoteLabel" flag through every wire type that carries a
 * project name (ProjectSummary, ingest.Session, QualitySession,
 * ChildSessionRef) is a real option but is disproportionate plumbing for a
 * rendering heuristic; the regex is deliberately narrow (colon THEN a
 * slash-free owner segment THEN a slash) so it does not swallow real
 * Windows paths (`C:\Users\...`, UNC `\\server\share`) or bare host-like
 * strings with no path — see the adversarial-shape cases in
 * quality/utils.test.ts locking that in. Revisit if a local project ever
 * legitimately needs a colon+slash name (tracked risk, not observed).
 */
const REMOTE_DISPLAY_LABEL = /^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?:[^\s/\\]+\/.+$/i;

/**
 * Extract a short, human-readable project name from a raw path, host slug, or
 * Claude-encoded path. Pure — never mutates; only formats for display.
 *
 * Decoding of Claude-encoded paths happens up-front so the segment recovery
 * always operates on a real path, never an opaque dash blob.
 *
 * Handles:
 *  - Remote-derived labels: "github.com:example-org/garden-app"             → unchanged
 *  - Host slugs:            "~Users-acme-dev-Documents-Projects-phaze"      → "phaze"
 *  - Filesystem paths:      "/Users/sampleuser/Documents/Projects/phaze"   → "phaze"
 *  - Windows paths:         "C:\\Users\\acme\\Projects\\phaze"              → "phaze"
 *  - Claude-encoded paths:  ".../-Users-sampleuser-Desktop-widget-demo"     → "widget-demo"
 *  - Bare home dir:         "/Users/sampleuser"                             → "sampleuser"
 */
export function displayProject(project: string): string {
  if (!project) return "";

  // A server-computed remote label already IS the display value; pass it
  // through unchanged before any path/basename heuristic can touch it.
  if (REMOTE_DISPLAY_LABEL.test(project)) return project;

  // Host slugs look like "~Users-acme-dev-Documents-Projects-phaze".
  const segments = project.split("-");
  if (segments.length > 1 && segments[0].startsWith("~")) {
    return segments[segments.length - 1];
  }

  // Take the last path segment for filesystem / Windows paths.
  let last = project;
  if (project.includes("/") || project.includes("\\")) {
    const seg = project.split(/[\\/]/).filter(Boolean).pop();
    if (seg) last = seg;
  }

  // A Claude-encoded path is the absolute path with separators turned into
  // dashes, e.g. "-Users-sampleuser-Desktop-widget-demo" (from
  // /Users/sampleuser/.claude/projects/...). Detect it (a leading optional dash
  // followed by letters then a dash) and recover the project folder by
  // dropping the well-known home prefix + common ancestor dirs, keeping the
  // remaining tail joined (so "w1-stc" survives, not just "stc").
  if (/^-?[A-Za-z]+-/.test(last)) {
    const parts = last.split("-").filter(Boolean);
    if (parts.length > 0) {
      let i = 0;
      // Drop the encoded home prefix: (Users|home)-<username>-
      if (
        parts.length >= 2 &&
        (parts[0] === "Users" || parts[0] === "home")
      ) {
        i = 2;
      }
      // Drop common ancestor directories that are never the project itself.
      while (i < parts.length - 1 && ANCESTOR_DIRS.has(parts[i])) i++;
      const tail = parts.slice(i);
      if (tail.length > 0) return tail.join("-");
      return parts[parts.length - 1];
    }
  }

  return last;
}

/**
 * Middle-truncate a path so the meaningful tail (the unique end of the path)
 * always survives. Collapses the home directory to `~` first, then drops middle
 * segments, replacing them with an ellipsis: `/Users/x/Documents/Projects/peasant`
 * → `~/…/Projects/peasant`.
 *
 * `maxLength` is the soft character budget; the result may be shorter when
 * fewer segments are needed and is never longer than the original.
 */
export function middleTruncatePath(path: string, maxLength = 36): string {
  if (!path) return "";

  // Collapse a "/Users/<name>" or "/home/<name>" prefix to "~".
  let display = path;
  const homeMatch = display.match(/^\/(Users|home)\/[^/]+(\/.*)?$/);
  if (homeMatch) {
    display = "~" + (homeMatch[2] ?? "");
  }

  if (display.length <= maxLength) return display;

  const sep = display.includes("\\") ? "\\" : "/";
  const segments = display.split(/[\\/]/);
  // Keep the first segment (root marker, "~", or drive) and as many trailing
  // segments as fit within the budget.
  if (segments.length <= 2) {
    // Nothing meaningful to drop — fall back to a head ellipsis.
    return "…" + display.slice(-(maxLength - 1));
  }

  const head = segments[0] || sep; // "" for an absolute path → leading sep
  const tail: string[] = [];
  // Walk segments from the end, accumulating until the budget is spent.
  for (let i = segments.length - 1; i >= 1; i--) {
    const candidate = [head, "…", ...tail, segments[i]].join(sep);
    if (candidate.length > maxLength && tail.length > 0) break;
    tail.unshift(segments[i]);
  }

  return [head, "…", ...tail].join(sep);
}
