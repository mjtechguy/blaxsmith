// Mount points for independently built components (setup checklists, the
// connection health line, the command palette). A feature module registers a
// component once at startup, e.g. in main.tsx:
//   registerSlot("home.checklist", HomeChecklist);
// and the shell/pages render <Slot name="home.checklist" ... /> wherever it belongs.
// An unregistered slot renders nothing.
import type { ComponentType } from "react";

export type SlotProps = {
  "home.checklist": { role: string };
  "admin.checklist": { role: string };
  "project.checklist": { projectId: string; role: string };
  "connection.health": { connectionId: string; scope: "organization" | "project" | "personal"; projectId?: string };
  "topbar.command": object;
};
export type SlotName = keyof SlotProps;

const registry = new Map<SlotName, ComponentType<never>>();

export function registerSlot<N extends SlotName>(name: N, component: ComponentType<SlotProps[N]>) {
  registry.set(name, component as ComponentType<never>);
}

export function hasSlot(name: SlotName) {
  return registry.has(name);
}

export function Slot<N extends SlotName>({ name, ...props }: { name: N } & SlotProps[N]) {
  const Component = registry.get(name) as ComponentType<SlotProps[N]> | undefined;
  return Component ? <div className="slot" data-slot={name}><Component {...(props as unknown as SlotProps[N])} /></div> : null;
}
