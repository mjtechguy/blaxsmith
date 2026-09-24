import type { ReactNode } from "react";

// Tables and dashboards use the full content width (up to 1800 px); forms are
// constrained inside their own layouts (editor, settings, create flow).
export function PageShell({ children }: { children: ReactNode }) {
  return <div className="page-shell">{children}</div>;
}

// The topbar breadcrumbs carry the hierarchy, so `eyebrow` is accepted for
// older call sites but no longer rendered above the title.
export function PageHeader({ title, description, actions }: {
  eyebrow?: string; title: string; description?: string; actions?: ReactNode;
}) {
  return <div className="page-header"><div>
    <h1>{title}</h1>
    {description ? <p className="page-description">{description}</p> : null}
  </div>{actions ? <div className="page-actions">{actions}</div> : null}</div>;
}
