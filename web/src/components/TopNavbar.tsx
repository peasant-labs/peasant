"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Moon, Search, Sun } from "lucide-react";
import { useTheme } from "@/hooks/useTheme";
import { Button } from "@/lib/ft-ui";
import { headerNavSections, isSectionActive } from "@/lib/nav/sections";
import { useServerCapabilities } from "@/contexts/ServerCapabilitiesContext";
import { OPEN_COMMAND_PALETTE_EVENT } from "@/components/command/CommandPalette";

/**
 * The local app header: one row, the `peasant` home link on the left, then
 * search, the nav sections other than home, and the theme toggle on the right.
 *
 * The links come from the section registry (lib/nav/sections.ts): a nav
 * section shows once this app has a page for it, and `home` is the brand link.
 * Analytics, changes and the code map are route-only sections, so nothing here
 * links to them; their routes still resolve. Connection state is not in the
 * header: the offline notice under it speaks only when the app is unreachable.
 */
export function TopNavbar() {
  const pathname = usePathname();
  const { theme, toggle } = useTheme();
  const { capabilities } = useServerCapabilities();
  const links = headerNavSections(capabilities);
  const nextTheme = theme === "light" ? "dark" : "light";

  return (
    <header className="fixed inset-x-0 top-0 z-50 flex h-[var(--nav-h)] items-center justify-between gap-3 border-b border-rule bg-surface px-4 grid-snap lg:px-8">
      <Link href="/" className="self-center focus-mono cursor-pointer" aria-label="Peasant home">
        <span className="font-[family-name:var(--font-display)] text-xl font-semibold text-ink">
          peasant
        </span>
      </Link>

      <div className="flex min-w-0 items-center gap-2 sm:gap-3">
        {/* Search: the visible way into the command palette, word plus shortcut.
            Its accessible name is its visible text; the shortcut is exposed as such. */}
        <Button
          size="sm"
          icon={Search}
          onClick={() => window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT))}
          title="search & jump (⌘K)"
          aria-keyshortcuts="Meta+K Control+K"
        >
          search
          <kbd className="kbd normal-case">⌘K</kbd>
        </Button>

        {/* Nav section links: fairtrade's small ghost button, as a client-side
            link so moving between sections keeps the socket. */}
        {links.map((section) => {
          const Icon = section.icon;
          return (
            <Link
              key={section.id}
              href={section.href}
              className="btn btn-ghost btn-sm"
              title={section.title}
              aria-current={isSectionActive(section, pathname) ? "page" : undefined}
            >
              {Icon ? <Icon size={14} aria-hidden="true" /> : null}
              {section.label}
            </Link>
          );
        })}

        {/* Theme: small and icon-only; the label names the mode it switches to. */}
        <Button
          size="sm"
          variant="ghost"
          icon={theme === "light" ? Moon : Sun}
          onClick={toggle}
          aria-label={`Switch to ${nextTheme} mode`}
          title={`Switch to ${nextTheme} mode`}
        />
      </div>
    </header>
  );
}
