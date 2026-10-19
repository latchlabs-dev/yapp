import { NavLink, Outlet, useLocation } from "react-router-dom";
import { FolderGit2, Gauge, LogOut, MessagesSquare } from "lucide-react";
import { useSession } from "@/hooks/use-session";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// `isActive` is computed rather than left to NavLink's `end`, because the chat
// section lives at two paths: `/` and `/c/:conversationId`. With `end` the tab
// went inactive as soon as a conversation was open — which is the only time the
// user is actually in it.
const NAV = [
  {
    to: "/",
    label: "Chat",
    icon: MessagesSquare,
    match: (path: string) => path === "/" || path.startsWith("/c/"),
  },
  {
    to: "/projects",
    label: "Projects",
    icon: FolderGit2,
    match: (path: string) => path.startsWith("/projects"),
  },
  {
    to: "/usage",
    label: "Usage",
    icon: Gauge,
    match: (path: string) => path.startsWith("/usage"),
  },
];

/**
 * AppShell is a full-height frame rather than a scrolling page: the chat view
 * owns its own scroll region, and a page-level scrollbar would fight it.
 *
 * The height unit is dvh, not vh: on phones vh counts the space behind the
 * browser's own chrome, which pushes the composer off the bottom of the screen.
 */
export function AppShell() {
  const { me, signOut, signingOut, signOutError } = useSession();
  const { pathname } = useLocation();

  return (
    <div className="flex h-dvh flex-col overflow-hidden">
      <header className="flex h-14 shrink-0 items-center gap-2 border-b border-border px-2 sm:gap-6 sm:px-6">
        <div className="flex items-center gap-2.5">
          <span className="grid grid-cols-2 gap-0.5" aria-hidden>
            <i className="h-1.5 w-1.5 rounded-[1px] bg-primary" />
            <i className="h-1.5 w-1.5 rounded-[1px] bg-primary/40" />
            <i className="h-1.5 w-1.5 rounded-[1px] bg-primary/40" />
            <i className="h-1.5 w-1.5 rounded-[1px] bg-primary" />
          </span>
          <span className="hidden font-display text-sm font-semibold tracking-tight sm:inline">
            Pi Console
          </span>
        </div>

        <nav className="flex min-w-0 items-center gap-0.5 sm:gap-1">
          {NAV.map(({ to, label, icon: Icon, match }) => (
            <NavLink
              key={to}
              to={to}
              aria-label={label}
              className={cn(
                "flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm transition-colors sm:px-2.5",
                match(pathname)
                  ? "bg-secondary text-foreground"
                  : "text-muted-foreground hover:bg-secondary/60 hover:text-foreground",
              )}
            >
              <Icon className="h-3.5 w-3.5 shrink-0" />
              {/* The labels are the first thing to go on a narrow screen: the
                  icons stay legible and the composer keeps its width. */}
              <span className="hidden sm:inline">{label}</span>
            </NavLink>
          ))}
        </nav>

        <div className="ml-auto flex min-w-0 items-center gap-3">
          <div className="hidden min-w-0 text-right sm:block">
            <div className="truncate text-xs font-medium leading-tight">
              {me?.user.display_name || me?.user.email}
            </div>
            {me?.tenant.name && (
              <div className="truncate text-[11px] leading-tight text-muted-foreground">
                {me.tenant.name}
              </div>
            )}
          </div>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void signOut()}
            aria-label="Sign out"
            aria-busy={signingOut}
            disabled={signingOut}
            className="shrink-0 text-muted-foreground hover:text-foreground"
          >
            <LogOut className="h-4 w-4" />
          </Button>
        </div>
      </header>
      {signOutError && (
        <p role="alert" className="border-b border-destructive/30 bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {signOutError}
        </p>
      )}

      <main className="min-h-0 flex-1 overflow-hidden">
        <Outlet />
      </main>
    </div>
  );
}
