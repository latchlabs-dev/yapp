import { lazy, Suspense, type ReactNode } from "react";
import { createBrowserRouter, Navigate, useLocation } from "react-router-dom";
import { useSession } from "@/hooks/use-session";
import { AppShell } from "@/components/layout/app-shell";

const LoginPage = lazy(() => import("@/pages/login").then((m) => ({ default: m.LoginPage })));
const ChatPage = lazy(() => import("@/pages/chat").then((m) => ({ default: m.ChatPage })));
const ProjectsPage = lazy(() => import("@/pages/projects").then((m) => ({ default: m.ProjectsPage })));
const MemoryPage = lazy(() => import("@/pages/memory").then((m) => ({ default: m.MemoryPage })));
const UsagePage = lazy(() => import("@/pages/usage").then((m) => ({ default: m.UsagePage })));

function PageLoader() {
  return <div className="p-6 text-sm text-muted-foreground">Loading…</div>;
}

function lazyPage(children: ReactNode) {
  return <Suspense fallback={<PageLoader />}>{children}</Suspense>;
}

/**
 * Protected waits for the session probe before deciding. The cookie is
 * HttpOnly, so there is nothing to read locally: rendering login early would
 * flash it at every already-signed-in user on every reload.
 */
function Protected({ children }: { children: ReactNode }) {
  const { me, loading } = useSession();
  const location = useLocation();

  if (loading) return <PageLoader />;
  if (!me) {
    const returnTo = encodeURIComponent(location.pathname + location.search);
    return <Navigate to={`/login?return=${returnTo}`} replace />;
  }
  return <>{children}</>;
}

export const router = createBrowserRouter([
  { path: "/login", element: lazyPage(<LoginPage />) },
  {
    path: "/",
    element: (
      <Protected>
        <AppShell />
      </Protected>
    ),
    children: [
      { index: true, element: lazyPage(<ChatPage />) },
      { path: "c/:conversationId", element: lazyPage(<ChatPage />) },
      { path: "projects", element: lazyPage(<ProjectsPage />) },
      { path: "projects/:projectId/memory", element: lazyPage(<MemoryPage />) },
      { path: "usage", element: lazyPage(<UsagePage />) },
    ],
  },
]);
