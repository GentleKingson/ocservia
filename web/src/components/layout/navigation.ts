import {
  Boxes,
  FlaskConical,
  LayoutDashboard,
  ListChecks,
  ScrollText,
  Settings,
  ShieldCheck,
} from "@lucide/vue";

import { developmentRuntime } from "../../shared/routes";

export const navigationLinks = [
  { to: "/", label: "overview", icon: LayoutDashboard },
  { to: "/nodes", label: "nodes", icon: Boxes },
  { to: "/operations", label: "operations", icon: ListChecks },
  { to: "/approvals", label: "approvals", icon: ShieldCheck },
  { to: "/audit", label: "audit", icon: ScrollText },
  ...(developmentRuntime
    ? [{ to: "/dev", label: "development", icon: FlaskConical }]
    : []),
  { to: "/settings", label: "settings", icon: Settings },
];

// Detail routes are not nested records, so prefix matching keeps their
// section marked as current.
export function isCurrentSection(to: string, path: string): boolean {
  if (to === "/") return path === "/";
  return path === to || path.startsWith(`${to}/`);
}
