import { Outlet } from "react-router-dom";
import { Sidebar } from "./sidebar";
import { CommandPalette } from "@/components/command-palette";

export function AppShell() {
  return (
    <div className="min-h-screen">
      <Sidebar />
      {/* The shell owns horizontal padding; pages must not add their own, or
          headings stop lining up from one page to the next. */}
      <main className="lg:pl-60">
        <div className="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-10 lg:py-8">
          <Outlet />
        </div>
      </main>
      <CommandPalette />
    </div>
  );
}
