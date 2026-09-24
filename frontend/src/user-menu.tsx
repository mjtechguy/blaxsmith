// Top-right account menu (WAI-ARIA menu button): identity, Account settings,
// My connections, theme, the command palette hint, and Sign out.
import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Cable, Check, Command, LogOut, Monitor, Moon, Settings, Sun } from "lucide-react";
import { initials, useAccount } from "./account";
import { announceSessionChange, clearWorkspaceCache, logout, sessionQueryKey } from "./auth";
import { setPrefs, usePrefs, type Theme } from "./preferences";
import { hasSlot } from "./slots";

const roleLabel: Record<string, string> = { owner: "Owner", admin: "Admin", member: "Member", viewer: "Viewer" };
const themes: Array<{ id: Theme; label: string; icon: typeof Sun }> = [{ id: "light", label: "Light", icon: Sun }, { id: "dark", label: "Dark", icon: Moon }, { id: "system", label: "System", icon: Monitor }];

export function UserMenu() {
  const { account } = useAccount();
  const { theme } = usePrefs();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [isOpen, setOpen] = useState(false);
  const [signingOut, setSigningOut] = useState(false);
  const [signOutError, setSignOutError] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const id = useId();
  const name = account?.displayName || account?.username || "Your account";

  const items = () => [...(menu.current?.querySelectorAll<HTMLElement>('[role^="menuitem"]:not([aria-disabled="true"])') ?? [])];
  const focusItem = (index: number) => { const list = items(); list[(index + list.length) % list.length]?.focus(); };
  const close = (returnFocus = true) => { setOpen(false); if (returnFocus) button.current?.focus(); };
  const openMenu = (at: "first" | "last") => { setOpen(true); requestAnimationFrame(() => focusItem(at === "first" ? 0 : -1)); };

  useEffect(() => {
    if (!isOpen) return;
    const outside = (event: MouseEvent) => { if (!menu.current?.contains(event.target as Node) && !button.current?.contains(event.target as Node)) setOpen(false); };
    document.addEventListener("mousedown", outside);
    return () => document.removeEventListener("mousedown", outside);
  }, [isOpen]);

  const onMenuKey = (event: KeyboardEvent<HTMLDivElement>) => {
    const list = items();
    const index = list.indexOf(document.activeElement as HTMLElement);
    if (event.key === "ArrowDown") { event.preventDefault(); focusItem(index + 1); }
    else if (event.key === "ArrowUp") { event.preventDefault(); focusItem(index - 1); }
    else if (event.key === "Home") { event.preventDefault(); focusItem(0); }
    else if (event.key === "End") { event.preventDefault(); focusItem(-1); }
    else if (event.key === "Escape") { event.preventDefault(); close(); }
    else if (event.key === "Tab") close(false);
  };

  async function signOut() {
    if (signingOut) return;
    setSigningOut(true);
    setSignOutError(false);
    try {
      await logout();
      await queryClient.cancelQueries();
      await clearWorkspaceCache(queryClient);
      queryClient.setQueryData(sessionQueryKey, null);
      announceSessionChange();
      await navigate({ to: "/login", search: { next: "/" }, replace: true });
    } catch {
      setSignOutError(true);
    } finally {
      setSigningOut(false);
    }
  }
  const go = (to: "/me/settings" | "/me/connections") => { close(false); void navigate({ to }); };

  return <div className="user-menu">
    <button ref={button} type="button" className="avatar-button" id={`${id}-button`} aria-haspopup="menu" aria-expanded={isOpen} aria-controls={`${id}-menu`}
      aria-label={`Account menu for ${name}`} onClick={() => (isOpen ? close() : openMenu("first"))}
      onKeyDown={(event) => { if (event.key === "ArrowDown") { event.preventDefault(); openMenu("first"); } if (event.key === "ArrowUp") { event.preventDefault(); openMenu("last"); } }}>
      <span className="avatar" aria-hidden="true">{initials(account)}</span>
      <span className="avatar-name">{name}</span>
    </button>
    {signOutError ? <span className="signout-error" role="alert">Sign-out could not be completed. Please try again.</span> : null}
    {isOpen ? <div ref={menu} id={`${id}-menu`} className="user-menu-panel" role="menu" aria-labelledby={`${id}-button`} onKeyDown={onMenuKey}>
      <div className="user-menu-identity" role="presentation">
        <span className="avatar avatar-large" aria-hidden="true">{initials(account)}</span>
        <span><strong>{name}</strong><small>{account?.username ? `@${account.username}` : "Signed in"}</small>
          <small>{account?.organizationName || "Organization"} · {roleLabel[account?.role ?? ""] ?? account?.role}</small></span>
      </div>
      <div className="user-menu-separator" role="separator" />
      <button type="button" role="menuitem" tabIndex={-1} className="user-menu-item" onClick={() => go("/me/settings")}><Settings size={15} aria-hidden="true" /> Account settings</button>
      <button type="button" role="menuitem" tabIndex={-1} className="user-menu-item" onClick={() => go("/me/connections")}><Cable size={15} aria-hidden="true" /> My connections</button>
      <div className="user-menu-separator" role="separator" />
      <div role="group" aria-label="Theme" className="user-menu-group">
        <span className="user-menu-label" aria-hidden="true">Theme</span>
        {themes.map((t) => <button key={t.id} type="button" role="menuitemradio" aria-checked={theme === t.id} tabIndex={-1} className="user-menu-item"
          onClick={() => setPrefs({ theme: t.id })}><t.icon size={15} aria-hidden="true" /> {t.label}{theme === t.id ? <Check size={14} className="user-menu-check" aria-hidden="true" /> : null}</button>)}
      </div>
      {hasSlot("topbar.command") ? <>
        <div className="user-menu-separator" role="separator" />
        <p className="user-menu-hint" role="presentation"><Command size={13} aria-hidden="true" /> Press <kbd>⌘</kbd> <kbd>K</kbd> (<kbd>Ctrl</kbd> <kbd>K</kbd>) to jump anywhere</p>
      </> : null}
      <div className="user-menu-separator" role="separator" />
      <button type="button" role="menuitem" tabIndex={-1} className="user-menu-item" aria-disabled={signingOut || undefined} onClick={() => void signOut()}>
        <LogOut size={15} aria-hidden="true" /> {signingOut ? "Signing out…" : "Sign out"}</button>
    </div> : null}
  </div>;
}
