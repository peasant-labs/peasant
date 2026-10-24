'use client';

import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { Check } from 'lucide-react';
import { cn } from '@/lib/utils';

/** One pickable row: a radio item inside its group. */
export interface ChoiceItem {
  value: string;
  label: ReactNode;
  /** The row's accessible name when `label` is not plain text. */
  name?: string;
  /** User content (a project name): shown in its own case, never lowercased. */
  keepCase?: boolean;
}

/** A set of mutually exclusive rows under an optional caption. */
export interface ChoiceGroup {
  id: string;
  caption?: string;
  value: string;
  items: readonly ChoiceItem[];
  onChoose: (value: string) => void;
  /** A line under the rows, such as `loading project choices…`. */
  note?: string;
  /** An action row under the rows, such as a retry after a failed read. */
  retry?: ChoiceAction;
}

/** A plain action row after the groups, such as `clear filters`. */
export interface ChoiceAction {
  label: string;
  onSelect: () => void;
  disabled?: boolean;
}

export interface ChoiceMenuProps {
  /** The trigger's visible content, which is also its accessible name. */
  trigger: ReactNode;
  /** The menu's accessible name. */
  ariaLabel: string;
  groups: readonly ChoiceGroup[];
  actions?: readonly ChoiceAction[];
  align?: 'start' | 'end';
  /** Runs each time the menu opens, before the rows show. */
  onOpen?: () => void;
  /** Close after a pick (a view switch) or stay open (stacked filters). */
  closeOnChoose?: boolean;
  className?: string;
}

/**
 * A dropdown of radio rows, drawn with fairtrade's menu classes
 * (`.menu-anchor`, `.menu-trigger`, `.menu-pop`, `.menu-item`): fairtrade's
 * `Menu` runs commands, and these rows hold a choice, so each row is a
 * `menuitemradio` that says whether it is the current one. The keyboard
 * follows fairtrade's `Menu`: Down/Up open on the first/last row, arrows move,
 * Home/End jump, Escape closes and returns focus, Tab and an outside click close.
 */
export function ChoiceMenu({ trigger, ariaLabel, groups, actions = [], align = 'start', onOpen, closeOnChoose = true, className }: ChoiceMenuProps) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const popRef = useRef<HTMLDivElement>(null);
  const menuId = useId();

  const rows = () => [...(popRef.current?.querySelectorAll<HTMLElement>('[data-choice-row]:not([aria-disabled="true"])') ?? [])];
  const focusRow = (which: 'first' | 'last' | 'checked') => {
    requestAnimationFrame(() => {
      const all = rows();
      const checked = all.find((row) => row.getAttribute('aria-checked') === 'true');
      const target = which === 'checked' ? checked ?? all[0] : which === 'last' ? all.at(-1) : all[0];
      target?.focus();
    });
  };
  const show = (which: 'first' | 'last' | 'checked') => {
    onOpen?.();
    setOpen(true);
    focusRow(which);
  };
  const close = (returnFocus: boolean) => {
    setOpen(false);
    if (returnFocus) triggerRef.current?.focus();
  };

  useEffect(() => {
    if (!open) return;
    const outside = (event: MouseEvent) => {
      const target = event.target as Node;
      if (!popRef.current?.contains(target) && !triggerRef.current?.contains(target)) setOpen(false);
    };
    document.addEventListener('mousedown', outside);
    return () => document.removeEventListener('mousedown', outside);
  }, [open]);

  const onTriggerKey = (event: React.KeyboardEvent) => {
    if (event.key === 'ArrowDown') { event.preventDefault(); show('first'); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); show('last'); }
    else if (event.key === 'Escape' && open) { event.preventDefault(); close(true); }
  };
  const onMenuKey = (event: React.KeyboardEvent) => {
    const all = rows();
    const at = all.indexOf(document.activeElement as HTMLElement);
    if (event.key === 'Escape') { event.preventDefault(); close(true); }
    else if (event.key === 'Tab') close(false);
    else if (!all.length) return;
    else if (event.key === 'ArrowDown') { event.preventDefault(); all[(at + 1) % all.length].focus(); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); all[(at - 1 + all.length) % all.length].focus(); }
    else if (event.key === 'Home') { event.preventDefault(); all[0].focus(); }
    else if (event.key === 'End') { event.preventDefault(); all[all.length - 1].focus(); }
  };
  const activate = (run: () => void) => (event: React.KeyboardEvent) => {
    if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); run(); }
  };

  return (
    <div className={cn('menu-anchor choice-menu', className)}>
      <button
        ref={triggerRef}
        type="button"
        className="btn btn-secondary menu-trigger choice-menu-trigger"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={menuId}
        onClick={() => (open ? close(true) : show('checked'))}
        onKeyDown={onTriggerKey}
      >
        {trigger}
      </button>
      <div ref={popRef} id={menuId} className="menu-pop menu-float choice-menu-pop" data-align={align} hidden={!open}>
        <div role="menu" aria-label={ariaLabel} onKeyDown={onMenuKey}>
          {groups.map((group, index) => (
            <div key={group.id}>
              {index > 0 && <div role="separator" className="menu-sep" />}
              {group.caption && <p className="menu-cap" aria-hidden="true">{group.caption}</p>}
              <ul role="group" aria-label={group.caption ?? ariaLabel} className="menu-list choice-menu-list">
                {group.items.map((item) => {
                  const checked = item.value === group.value;
                  const choose = () => {
                    group.onChoose(item.value);
                    if (closeOnChoose) close(true);
                  };
                  return (
                    <li
                      key={item.value}
                      role="menuitemradio"
                      aria-checked={checked}
                      aria-label={item.name}
                      tabIndex={-1}
                      data-choice-row=""
                      className="menu-item choice-menu-item"
                      onClick={choose}
                      onKeyDown={activate(choose)}
                    >
                      <span className="choice-menu-check" aria-hidden="true">{checked && <Check />}</span>
                      <span className={cn('menu-text', item.keepCase && 'choice-menu-own-case')}>{item.label}</span>
                    </li>
                  );
                })}
                {group.retry && <ActionRow action={group.retry} close={close} activate={activate} />}
              </ul>
              {group.note && <p className="menu-cap choice-menu-note" role="status">{group.note}</p>}
            </div>
          ))}
          {actions.length > 0 && (
            <>
              <div role="separator" className="menu-sep" />
              <ul role="group" aria-label="actions" className="menu-list">
                {actions.map((action) => <ActionRow key={action.label} action={action} close={close} activate={activate} />)}
              </ul>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

interface ActionRowProps {
  action: ChoiceAction;
  close: (returnFocus: boolean) => void;
  activate: (run: () => void) => (event: React.KeyboardEvent) => void;
}

/** A command row in the menu: it runs, then the menu closes. */
function ActionRow({ action, close, activate }: ActionRowProps) {
  const run = () => {
    if (action.disabled) return;
    action.onSelect();
    close(true);
  };
  return (
    <li
      role="menuitem"
      tabIndex={-1}
      aria-disabled={action.disabled || undefined}
      data-choice-row=""
      className="menu-item choice-menu-item"
      onClick={run}
      onKeyDown={activate(run)}
    >
      <span className="choice-menu-check" aria-hidden="true" />
      <span className="menu-text">{action.label}</span>
    </li>
  );
}
