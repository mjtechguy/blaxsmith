import type { ReactNode } from "react";
import { Hammer, RefreshCw } from "lucide-react";
import { usePageTitle } from "./work-log";

export function AuthFrame({ children, title = "" }: { children: ReactNode; title?: string }) {
  usePageTitle(title);
  return <main className="auth-frame" id="main-content">
    <aside className="auth-story" aria-label="About Blaxsmith">
      <div className="auth-grid" aria-hidden="true" />
      <div className="auth-story-content">
        <div className="auth-brand"><span className="brand-mark"><Hammer size={18} aria-hidden="true" /></span><strong>Blaxsmith</strong></div>
        <div className="auth-message"><p className="auth-kicker">Engineering work, in one place</p><h1>Shape the work. Guide the agents. Review the result.</h1><p>Plan, build, verify, and hand off with a clear record of every decision.</p></div>
        <p className="auth-story-footer">A workspace for human-led agent teams.</p>
      </div>
    </aside>
    <section className="auth-main"><div className="auth-content">
      <div className="auth-mobile-brand"><span className="brand-mark"><Hammer size={17} aria-hidden="true" /></span><strong>Blaxsmith</strong></div>
      {children}
    </div></section>
  </main>;
}

export function AuthUnavailable({ retry }: { retry: () => void }) {
  return <AuthFrame><div className="auth-heading" role="alert"><p className="eyebrow">Connection needed</p><h2>Sign-in is unavailable</h2><p>The application identity service cannot be reached at this address. Try again or contact your administrator.</p></div><button className="secondary-button" type="button" onClick={retry}><RefreshCw size={16} aria-hidden="true" /> Try again</button></AuthFrame>;
}
