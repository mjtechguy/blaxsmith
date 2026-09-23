import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useLocation } from "@tanstack/react-router";
import { ChevronLeft, ChevronRight, Hammer, LayoutDashboard, Menu, Moon, Sun, Wrench, X } from "lucide-react";

type Theme = "light" | "dark" | "system";

function applyTheme(theme: Theme) {
  document.documentElement.classList.toggle("dark", theme === "dark" ||
    (theme === "system" && matchMedia("(prefers-color-scheme: dark)").matches));
}

export function Shell({ children }: { children: ReactNode }) {
  const pathname = useLocation({ select: (location) => location.pathname });
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
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
    { to: "/" as const, label: "Workspace", icon: LayoutDashboard },
    { to: "/tools" as const, label: "Tools & runtimes", icon: Wrench },
  ];
  const pageName = pathname === "/tools" ? "Tools & runtimes" : "Workspace";
  const nextTheme: Theme = theme === "light" ? "dark" : theme === "dark" ? "system" : "light";

  return <div className={`app-frame ${collapsed ? "is-collapsed" : ""} ${mobileOpen ? "mobile-open" : ""}`}>
    <a className="skip-link" href="#main-content">Skip to content</a>
    {mobileOpen ? <button className="mobile-backdrop" type="button" aria-label="Close navigation" onClick={() => { setMobileOpen(false); menuButton.current?.focus(); }} /> : null}
    <aside className="sidebar" aria-label="Primary navigation">
      <div className="sidebar-brand">
        <Link to="/" className="brand-link" aria-label="Blaxsmith home">
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
            <Link key={to} to={to} activeOptions={{ exact: true }} className="nav-link" activeProps={{ className: "nav-link active" }} title={collapsed ? label : undefined}>
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
          <button type="button" className="icon-button theme-button" onClick={() => setTheme(nextTheme)} aria-label={`Theme: ${theme}. Switch to ${nextTheme}`} title={`Theme: ${theme}`}>
            {theme === "dark" ? <Moon size={17} /> : <Sun size={17} />}
          </button>
        </div>
      </header>
      <main id="main-content" className="main-content" tabIndex={-1}>{children}</main>
    </div>
  </div>;
}
