import {
  LayoutDashboard,
  Building2,
  Blocks,
  Network,
  Gauge,
  BarChart3,
  MessageSquare,
  Brain,
  FolderGit2,
  TriangleAlert,
  Sparkles,
  Webhook,
  Globe,
  type LucideIcon,
} from "lucide-react";

export interface NavItem {
  to: string;
  icon: LucideIcon;
  /** Short label for the sidebar. */
  label: string;
  /** Longer, descriptive label for the command palette / search. */
  searchLabel: string;
  /** Match exactly (only "/" needs this so it doesn't prefix-match everything). */
  end?: boolean;
}

export interface NavGroup {
  /** Sidebar section heading; omitted for the ungrouped top entry. */
  label?: string;
  items: NavItem[];
}

// Single source of truth for primary navigation. Consumed by both the sidebar
// (sidebar.tsx) and the command palette so labels/routes can't drift. Groups
// follow what an operator is doing: who has access, where traffic goes, what
// happened, and what the fleet has learned.
export const navGroups: NavGroup[] = [
  {
    items: [
      { to: "/", icon: LayoutDashboard, label: "Overview", searchLabel: "Overview", end: true },
    ],
  },
  {
    label: "Access",
    items: [
      { to: "/tenants", icon: Building2, label: "Tenants", searchLabel: "Tenants" },
      { to: "/quotas", icon: Gauge, label: "Quotas", searchLabel: "Quotas" },
      { to: "/web-access", icon: Globe, label: "Web console", searchLabel: "Web Console Access" },
    ],
  },
  {
    label: "Routing",
    items: [
      { to: "/providers", icon: Blocks, label: "Providers", searchLabel: "Providers" },
      { to: "/model-aliases", icon: Network, label: "Models", searchLabel: "Synthetic Models" },
      { to: "/hooks", icon: Webhook, label: "Hooks", searchLabel: "Hook Subscriptions" },
    ],
  },
  {
    label: "Activity",
    items: [
      { to: "/usage", icon: BarChart3, label: "Usage", searchLabel: "Usage Reports" },
      { to: "/sessions", icon: MessageSquare, label: "Sessions", searchLabel: "Sessions" },
      { to: "/extension-errors", icon: TriangleAlert, label: "Errors", searchLabel: "Extension Errors" },
    ],
  },
  {
    label: "Knowledge",
    items: [
      { to: "/projects", icon: FolderGit2, label: "Projects", searchLabel: "Git Repository Projects" },
      { to: "/memory", icon: Brain, label: "Memory", searchLabel: "Advanced Memory Controls" },
      { to: "/insights", icon: Sparkles, label: "Insights", searchLabel: "Insight Services and Configurations" },
    ],
  },
];

export const navItems: NavItem[] = navGroups.flatMap((g) => g.items);
