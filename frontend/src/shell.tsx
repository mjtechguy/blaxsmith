import { useEffect, useRef, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import { ChevronLeft, ChevronRight, Hammer, KeyRound, LayoutDashboard, LogOut, Menu, Moon, ShieldCheck, Sun, Wrench, X } from "lucide-react";
import { isOrgAdmin } from "./admin";
import { CommandPalette } from "./command-palette";
import { announceSessionChange, clearWorkspaceCache, logout, sessionQueryKey } from "./auth";
import type { SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";

type Theme = "light" | "dark" | "system";

function applyTheme(theme: Theme) {
  document.documentElement.classList.toggle("dark", theme === "dark" ||
    (theme === "system" && matchMedia("(prefers-color-scheme: dark)").matches));
}

export function Shell({ children, session }: { children: ReactNode; session?: SessionIdentity }) {
  const pathname = useLocation({ select: (location) => location.pathname });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [signingOut, setSigningOut] = useState(false);
  const [signOutError, setSignOutError] = useState(false);
  const [theme, setTheme] = useState<Theme>(() => (localStorage.getItem("blaxsmith-theme") as Theme) || "system");
  const menuButton = useRef<HTMLButtonElement>(null);

  useEffect(() => { setMobileOpen(false); }, [pathname]);
  useEffect(() => {
    applyTheme(theme);
    localStorage.setItem("blaxsmith-theme", theme);
    const media = matchMedia("(prefers-color-scheme: dark)");
    const change = () => { if (theme === "system") applyTheme(theme); };
    media.addEventListener("change", change);
    return () => media.removeEventListener("change", change);
  }, [theme]);
  useEffect(() => {
    if (!mobileOpen) return;
    const close = (event: KeyboardEvent) => {
      if (event.key === "Escape") { setMobileOpen(false); menuButton.current?.focus(); }
    };
    window.addEventListener("keydown", close);
    return () => window.removeEventListener("keydown", close);
  }, [mobileOpen]);

  const nav = [
    ...(session ? [{ to: "/" as const, label: "Workspace", icon: LayoutDashboard }] : []),
    { to: "/tools" as const, label: "Tools & runtimes", icon: Wrench },
    ...(session ? [{ to: "/me/connections" as const, label: "My connections", icon: KeyRound }] : []),
    ...(isOrgAdmin(session) ? [{ to: "/admin" as const, label: "Admin", icon: ShieldCheck }] : []),
  ];
  const isActive = (to: string) => to === "/" || to === "/tools" ? pathname === to : pathname === to || pathname.startsWith(`${to}/`);
  const pageName = pathname === "/tools" ? "Tools & runtimes" : pathname.startsWith("/admin") ? "Admin" : pathname.includes("/connections") ? "Connections" : "Workspace";
  const nextTheme: Theme = theme === "light" ? "dark" : theme === "dark" ? "system" : "light";

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

  return <div className={`app-frame ${collapsed ? "is-collapsed" : ""} ${mobileOpen ? "mobile-open" : ""}`}>
    <a className="skip-link" href="#main-content">Skip to content</a>
    <aside className="sidebar" aria-label="Primary navigation">
      <div className="sidebar-brand">
        <Link to={session ? "/" : "/tools"} className="brand-link" aria-label="Blaxsmith home">
          <span className="brand-mark"><Hammer size={16} strokeWidth={2.3} aria-hidden="true" /></span>
          <span className="brand-copy"><strong>Blaxsmith</strong><small>Engineering workspace</small></span>
        </Link>
        <button type="button" className="icon-button collapse-button" onClick={() => setCollapsed(!collapsed)} aria-label={collapsed ? "Expand navigation" : "Collapse navigation"}>
          {collapsed ? <ChevronRight size={16} /> : <ChevronLeft size={16} />}
        </button>
        <button type="button" className="icon-button mobile-close" onClick={() => { setMobileOpen(false); menuButton.current?.focus(); }} aria-label="Close navigation"><X size={17} /></button>
      </div>
      <div className="sidebar-scroll">
        <p className="nav-caption">WORKSPACE</p>
        <nav aria-label="Main">
          {nav.map(({ to, label, icon: Icon }) =>
            <Link key={to} to={to} activeOptions={{ exact: true }} className={isActive(to) ? "nav-link active" : "nav-link"} aria-current={isActive(to) ? "page" : undefined} title={collapsed ? label : undefined}>
              <Icon size={17} aria-hidden="true" /><span>{label}</span>
            </Link>) }
        </nav>
      </div>
      <div className="sidebar-footer"><span className="footer-dot" /> <span>Development preview</span></div>
    </aside>
    <div className="main-column">
      <header className="topbar">
        <div className="topbar-left">
          <button type="button" ref={menuButton} className="icon-button menu-button" onClick={() => setMobileOpen(true)} aria-label="Open navigation" aria-expanded={mobileOpen}><Menu size={19} /></button>
          <div className="breadcrumb"><span>Blaxsmith</span><ChevronRight size={14} aria-hidden="true" /><strong>{pageName}</strong></div>
        </div>
        <div className="topbar-right">
          <span className="preview-pill">Preview</span>
          {session ? <CommandPalette session={session} /> : null}
          <button type="button" className="icon-button theme-button" onClick={() => setTheme(nextTheme)} aria-label={`Theme: ${theme}. Switch to ${nextTheme}`} title={`Theme: ${theme}`}>
            {theme === "dark" ? <Moon size={17} /> : <Sun size={17} />}
          </button>
          {session ? <><span className="account-role" title={`Signed in as ${session.role}`}>{session.role}</span>
            <button type="button" className="secondary-button sign-out" onClick={() => void signOut()} disabled={signingOut}><LogOut size={15} aria-hidden="true" />{signingOut ? "Signing out…" : "Sign out"}</button></> :
            <Link to="/login" search={{ next: "/tools" }} className="secondary-button">Sign in</Link>}
        </div>
      </header>
      {signOutError ? <div className="account-error" role="alert">Sign-out could not be completed. Please try again.</div> : null}
      <main id="main-content" className="main-content" tabIndex={-1}>{children}</main>
    </div>
  </div>;
}
