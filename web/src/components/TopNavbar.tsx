"use client";

import { useTheme } from "@/hooks/useTheme";
import { cn } from "@/lib/utils";
import { Button } from "@/lib/ft-ui";
import { GraphSectionNav } from "@peasant-labs/fairtrade/graph";
import { Share2 } from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { NAV_SECTIONS, isSectionActive, type NavSection } from "@/lib/nav/sections";
import { CONNECTION } from "@/components/ConnectionState";
import { OPEN_COMMAND_PALETTE_EVENT } from "@/components/command/CommandPalette";

interface TopNavbarProps {
  connected: boolean;
}

export function TopNavbar({ connected }: TopNavbarProps) {
  const pathname = usePathname();
  const normalizedPathname = pathname.replace(/\/+$/, "") || "/";
  const { theme, toggle } = useTheme();

  // Sections come from the shared IA module — one source for the nav,
  // breadcrumbs, and (future) Cmd+K palette.
  const links = NAV_SECTIONS;
  const activeSection = links.find((link) => isSectionActive(link, pathname));

  return (
    <header className="fixed top-0 left-0 right-0 z-50 h-[var(--app-header-height)] border-b border-rule bg-surface grid-snap">
      <div className="grid h-full grid-cols-[auto_minmax(0,1fr)] grid-rows-[3.5rem_3.5rem] px-4 lg:flex lg:items-center lg:justify-between lg:px-8">
        {/* Logo + Nav */}
        <div className="contents lg:flex lg:items-center lg:gap-6">
          <Link href="/" className="self-center focus-mono cursor-pointer" aria-label="Peasant home">
            <span className="font-[family-name:var(--font-display)] text-xl font-semibold text-ink">
              peasant
            </span>
          </Link>

          <GraphSectionNav
            sections={links}
            activeId={activeSection?.id}
            hrefFor={(link: NavSection) => link.href}
            LinkComponent={Link}
            className="col-span-2 row-start-2 flex min-w-0 items-center justify-center gap-0.5 border-t border-rule lg:border-0"
            // Matches the fairtrade in-use demo's `.iu-subnav-item` / `.iu-subnav-item.active`
            // (a filled amber pill — color: on-amber, background: amber, border-color: amber)
            // rather than an underline marker. `border-transparent` on the base item keeps the
            // border width constant so the active pill doesn't shift layout when it fills in.
            // itemClassName REPLACES the DS class, so the DS chrome typography
            // (mono, lowercase) must ride along — dropping it left the nav sans.
            itemClassName="border border-transparent px-3 py-1.5 text-sm font-medium font-[family-name:var(--font-mono)] lowercase transition-colors duration-150 focus-mono cursor-pointer text-ink-3 hover:text-ink hover:bg-surface-hover"
            activeItemClassName="bg-amber text-on-amber border-amber"
            ariaLabel="Main navigation"
          />
        </div>

        {/* Right cluster: connection status + command palette + theme toggle.
            The connection pill leads (least prominent, de-emphasized) so the
            interactive controls anchor the right edge. */}
        <div className="col-start-2 row-start-1 flex min-w-0 items-center justify-end gap-2 sm:gap-3">
          {/* Connection status — quiet, glanceable. In the steady connected
              state it has NO box border, so it reads as a plain status glance
              rather than a bordered pill competing with the controls. The
              bordered + colored treatment is reserved for the disconnected
              (needs-attention) state. This is the one persistent connection
              indicator; pages don't repeat it (see ConnectionState.tsx). The
              connected dot is ink, not green — being connected is a steady
              state, not a success outcome. */}
          <span
            title={connected ? CONNECTION.liveTitle : CONNECTION.connectingTitle}
            className={cn(
              "hidden sm:inline-flex items-center gap-1.5 font-mono text-xs cursor-help",
              connected
                ? "text-ink-3"
                : "border border-danger/30 bg-danger-soft px-2.5 py-1 text-danger"
            )}
          >
            <span
              className={cn("h-1.5 w-1.5", connected ? "bg-ink" : "bg-danger")}
              aria-hidden
            />
            {connected ? CONNECTION.liveLabel : CONNECTION.connectingLabel}
          </span>

          {/* Command palette affordance — makes the Cmd-K shortcut discoverable
              so keyboard-first navigation is not hidden. Shows the
              word "Search" beside the shortcut so it reads as a search
              affordance, not a bare icon. */}
          <button
            onClick={() => window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT))}
            title="search & jump (⌘K)"
            aria-label="Open the command palette (Command or Control + K)"
            className="hidden sm:inline-flex items-center gap-1.5 border border-rule px-2 py-1 text-ink-3 transition-colors duration-150 hover:text-ink hover:bg-surface-hover focus-mono cursor-pointer"
          >
            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
              <circle cx="11" cy="11" r="7" />
              <path d="m21 21-4.3-4.3" />
            </svg>
            <span className="text-sm">search</span>
            <kbd className="font-mono text-[11px]">⌘K</kbd>
          </button>

          {/* Share is the persistent, consequential app action. It stays outside
              the three graph sections, whose order and membership come from
              Fairtrade, and leads to the existing review-and-share route. */}
          <Button
            as="a"
            href="/share"
            variant="primary"
            size="sm"
            icon={Share2}
            aria-current={normalizedPathname === "/share" ? "page" : undefined}
          >
            share
          </Button>

          {/* Theme toggle — square, monochrome */}
          <button
            onClick={toggle}
            className="flex h-8 w-8 items-center justify-center text-ink-3 transition-colors duration-150 hover:text-ink hover:bg-surface-hover focus-mono cursor-pointer"
            aria-label={`Switch to ${theme === "light" ? "dark" : "light"} mode`}
            title={`Switch to ${theme === "light" ? "dark" : "light"} mode`}
          >
            {theme === "light" ? (
              /* Moon icon */
              <svg
                width="15"
                height="15"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden
              >
                <path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z" />
              </svg>
            ) : (
              /* Sun icon */
              <svg
                width="15"
                height="15"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden
              >
                <circle cx="12" cy="12" r="4" />
                <path d="M12 2v2" />
                <path d="M12 20v2" />
                <path d="m4.93 4.93 1.41 1.41" />
                <path d="m17.66 17.66 1.41 1.41" />
                <path d="M2 12h2" />
                <path d="M20 12h2" />
                <path d="m6.34 17.66-1.41 1.41" />
                <path d="m19.07 4.93-1.41 1.41" />
              </svg>
            )}
          </button>
        </div>
      </div>
    </header>
  );
}
