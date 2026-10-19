import { useState } from "react";
import { NavLink, useLocation } from "react-router-dom";
import { LogOut, Menu, Search } from "lucide-react";
import { cn } from "@/lib/utils";
import { navGroups } from "@/lib/nav";
import { useAuth } from "@/hooks/use-auth";
import { Sheet, SheetContent, SheetTitle } from "@/components/ui/sheet";

function openCommandPalette() {
  document.dispatchEvent(new KeyboardEvent("keydown", { key: "k", metaKey: true }));
}

function Brand() {
  return (
    <a href="/" className="flex items-center gap-2.5">
      <span className="flex h-7 w-7 items-center justify-center rounded-md bg-primary/12 ring-1 ring-inset ring-primary/30">
        <span className="grid grid-cols-2 gap-[3px]">
          <i className="h-[4px] w-[4px] rounded-[1px] bg-primary" />
          <i className="h-[4px] w-[4px] rounded-[1px] bg-primary/40" />
          <i className="h-[4px] w-[4px] rounded-[1px] bg-primary/40" />
          <i className="h-[4px] w-[4px] rounded-[1px] bg-primary" />
        </span>
      </span>
      <span className="font-display text-[15px] font-semibold tracking-tight text-foreground">
        PiProxy
      </span>
      <span className="eyebrow !text-[10px] !tracking-[0.1em] text-muted-foreground/80">Admin</span>
    </a>
  );
}

/** NavList renders the grouped navigation; shared by the desktop rail and the phone drawer. */
function NavList({ onNavigate }: { onNavigate?: () => void }) {
  const { pathname } = useLocation();
  const isActive = (to: string, end?: boolean) =>
    end ? pathname === to : pathname === to || pathname.startsWith(`${to}/`);

  return (
    <nav className="space-y-5">
      {navGroups.map((group, gi) => (
        <div key={group.label ?? gi}>
          {group.label && (
            <div className="mb-1.5 px-2.5 text-[11px] font-medium text-muted-foreground/70">
              {group.label}
            </div>
          )}
          <ul className="space-y-px">
            {group.items.map((item) => {
              const active = isActive(item.to, item.end);
              return (
                <li key={item.to}>
                  <NavLink
                    to={item.to}
                    end={item.end}
                    onClick={onNavigate}
                    aria-current={active ? "page" : undefined}
                    className={cn(
                      "relative flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[13px] font-medium transition-colors",
                      active
                        ? "bg-secondary text-foreground"
                        : "text-muted-foreground hover:bg-secondary/50 hover:text-foreground",
                    )}
                  >
                    {active && (
                      <span className="absolute inset-y-1.5 -left-3 w-0.5 rounded-full bg-primary" />
                    )}
                    <item.icon
                      className={cn("h-4 w-4 shrink-0", active ? "text-primary" : "opacity-80")}
                    />
                    {item.label}
                  </NavLink>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );
}

function SearchButton() {
  return (
    <button
      type="button"
      onClick={openCommandPalette}
      className="flex w-full items-center gap-2 rounded-md border border-border bg-background/60 px-2.5 py-1.5 text-xs text-muted-foreground transition-colors hover:border-primary/40 hover:text-foreground"
    >
      <Search className="h-3.5 w-3.5" />
      <span>Search</span>
      <kbd className="ml-auto rounded border border-border bg-secondary/60 px-1 font-mono text-[10px]">
        ⌘K
      </kbd>
    </button>
  );
}

function SidebarFooter() {
  const { logout } = useAuth();
  return (
    <div className="flex items-center justify-between border-t border-border px-5 py-3">
      <span className="flex items-center gap-2">
        <span className="led" />
        <span className="text-xs text-muted-foreground">Live</span>
      </span>
      <button
        onClick={logout}
        className="inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium text-muted-foreground transition-colors hover:bg-secondary hover:text-foreground"
      >
        <LogOut className="h-3.5 w-3.5" />
        Sign out
      </button>
    </div>
  );
}

/**
 * Sidebar is the admin's primary navigation: a fixed rail from lg up and a
 * drawer behind a slim top bar below that. Fourteen destinations do not fit a
 * horizontal tab strip without scrolling, and grouping them tells an operator
 * where a page lives before they have learned its name.
 */
export function Sidebar() {
  const [open, setOpen] = useState(false);

  return (
    <>
      <aside className="fixed inset-y-0 left-0 z-40 hidden w-60 flex-col border-r border-border bg-card/40 backdrop-blur-xl lg:flex">
        <div className="flex h-14 shrink-0 items-center px-5">
          <Brand />
        </div>
        <div className="px-3 pb-3">
          <SearchButton />
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-3 pb-4 pt-2">
          <NavList />
        </div>
        <SidebarFooter />
      </aside>

      <header className="sticky top-0 z-40 flex h-14 items-center gap-3 border-b border-border bg-background/85 px-4 backdrop-blur-xl lg:hidden">
        <button
          type="button"
          onClick={() => setOpen(true)}
          aria-label="Open navigation"
          className="-ml-1.5 rounded-md p-1.5 text-muted-foreground hover:bg-secondary hover:text-foreground"
        >
          <Menu className="h-5 w-5" />
        </button>
        <Brand />
        <button
          type="button"
          onClick={openCommandPalette}
          aria-label="Search"
          className="ml-auto rounded-md p-1.5 text-muted-foreground hover:bg-secondary hover:text-foreground"
        >
          <Search className="h-4 w-4" />
        </button>
      </header>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="left" className="flex w-64 flex-col gap-0 p-0">
          <SheetTitle className="sr-only">Navigation</SheetTitle>
          <div className="flex h-14 shrink-0 items-center px-5">
            <Brand />
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto px-3 pb-4 pt-2">
            <NavList onNavigate={() => setOpen(false)} />
          </div>
          <SidebarFooter />
        </SheetContent>
      </Sheet>
    </>
  );
}
