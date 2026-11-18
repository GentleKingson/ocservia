import {
  Boxes,
  LayoutDashboard,
  ListChecks,
  ScrollText,
  Settings,
} from "@lucide/vue";

export const navigationLinks = [
  { to: "/", label: "overview", icon: LayoutDashboard },
  { to: "/nodes", label: "nodes", icon: Boxes },
  { to: "/operations", label: "operations", icon: ListChecks },
  { to: "/audit", label: "audit", icon: ScrollText },
  { to: "/settings", label: "settings", icon: Settings },
];

// Detail routes are not nested records, so prefix matching keeps their
// section marked as current.
export function isCurrentSection(to: string, path: string): boolean {
  if (to === "/") return path === "/";
  return path === to || path.startsWith(`${to}/`);
}
