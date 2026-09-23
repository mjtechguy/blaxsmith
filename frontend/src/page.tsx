import type { ReactNode } from "react";

export function PageShell({ children }: { children: ReactNode }) {
  return <div className="page-shell">{children}</div>;
}

export function PageHeader({ eyebrow, title, description, actions }: {
  eyebrow?: string; title: string; description?: string; actions?: ReactNode;
}) {
  return <div className="page-header"><div>
    {eyebrow ? <p className="eyebrow">{eyebrow}</p> : null}
    <h1>{title}</h1>
    {description ? <p className="page-description">{description}</p> : null}
  </div>{actions ? <div className="page-actions">{actions}</div> : null}</div>;
}
