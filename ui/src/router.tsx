import { lazy, Suspense, type ReactNode } from "react";
import { createBrowserRouter, Navigate } from "react-router-dom";
import { useAuth } from "@/hooks/use-auth";
import { AppShell } from "@/components/layout/app-shell";
import { RouteError } from "@/components/route-error";

const LoginPage = lazy(() => import("@/pages/login").then((m) => ({ default: m.LoginPage })));
const DashboardPage = lazy(() => import("@/pages/dashboard").then((m) => ({ default: m.DashboardPage })));
const TenantsListPage = lazy(() => import("@/pages/tenants/list").then((m) => ({ default: m.TenantsListPage })));
const TenantDetailPage = lazy(() => import("@/pages/tenants/detail").then((m) => ({ default: m.TenantDetailPage })));
const ProvidersListPage = lazy(() => import("@/pages/providers/list").then((m) => ({ default: m.ProvidersListPage })));
const ProviderDetailPage = lazy(() => import("@/pages/providers/detail").then((m) => ({ default: m.ProviderDetailPage })));
const ModelAliasesPage = lazy(() => import("@/pages/model-aliases").then((m) => ({ default: m.ModelAliasesPage })));
const QuotasPage = lazy(() => import("@/pages/quotas").then((m) => ({ default: m.QuotasPage })));
const UserQuotaDetailPage = lazy(() => import("@/pages/quotas/user-detail").then((m) => ({ default: m.UserQuotaDetailPage })));
const UsagePage = lazy(() => import("@/pages/usage").then((m) => ({ default: m.UsagePage })));
const ModelDetailPage = lazy(() => import("@/pages/model-detail").then((m) => ({ default: m.ModelDetailPage })));
const ConversationPayloadsPage = lazy(() => import("@/pages/conversation-payloads").then((m) => ({ default: m.ConversationPayloadsPage })));
const SessionsPage = lazy(() => import("@/pages/sessions").then((m) => ({ default: m.SessionsPage })));
const ExtensionErrorsPage = lazy(() => import("@/pages/extension-errors").then((m) => ({ default: m.ExtensionErrorsPage })));
const MemoryPage = lazy(() => import("@/pages/memory").then((m) => ({ default: m.MemoryPage })));
const ProjectsPage = lazy(() => import("@/pages/projects").then((m) => ({ default: m.ProjectsPage })));
const InsightsPage = lazy(() => import("@/pages/insights").then((m) => ({ default: m.InsightsPage })));
const HooksPage = lazy(() => import("@/pages/hooks").then((m) => ({ default: m.HooksPage })));
const WebAccessPage = lazy(() => import("@/pages/web-access").then((m) => ({ default: m.WebAccessPage })));

function PageLoader() {
  return (
    <div className="p-6 text-sm text-muted-foreground">
      Loading…
    </div>
  );
}

function lazyPage(children: ReactNode) {
  return <Suspense fallback={<PageLoader />}>{children}</Suspense>;
}

function ProtectedRoute({ children }: { children: ReactNode }) {
  const { adminKey, initializing } = useAuth();

  // Don't redirect while we're still verifying a stored key — otherwise a
  // remembered session flashes the login screen on every reload.
  if (initializing) {
    return null;
  }
  if (adminKey === null) {
    return <Navigate to="/login" replace />;
  }
  return <>{children}</>;
}

export const router = createBrowserRouter([
  {
    path: "/login",
    element: lazyPage(<LoginPage />),
  },
  {
    path: "/",
    element: (
      <ProtectedRoute>
        <AppShell />
      </ProtectedRoute>
    ),
    // errorElement is attached per-child below so a page fault renders inside
    // the shell (the sidebar stays) instead of replacing the whole layout.
    children: [
      { index: true, element: lazyPage(<DashboardPage />) },
      { path: "tenants", element: lazyPage(<TenantsListPage />) },
      { path: "tenants/:tenantId", element: lazyPage(<TenantDetailPage />) },
      { path: "providers", element: lazyPage(<ProvidersListPage />) },
      { path: "providers/:accountId", element: lazyPage(<ProviderDetailPage />) },
      { path: "model-aliases", element: lazyPage(<ModelAliasesPage />) },
      { path: "quotas", element: lazyPage(<QuotasPage />) },
      { path: "quotas/:tenantId/:userId", element: lazyPage(<UserQuotaDetailPage />) },
      { path: "usage", element: lazyPage(<UsagePage />) },
      { path: "models/:modelId", element: lazyPage(<ModelDetailPage />) },
      { path: "conversation-payloads", element: lazyPage(<ConversationPayloadsPage />) },
      { path: "sessions/captured/:chatKey", element: lazyPage(<ConversationPayloadsPage />) },
      { path: "sessions", element: lazyPage(<SessionsPage />) },
      { path: "sessions/:conversationId", element: lazyPage(<SessionsPage />) },
      { path: "extension-errors", element: lazyPage(<ExtensionErrorsPage />) },
      { path: "projects", element: lazyPage(<ProjectsPage />) },
      { path: "projects/:projectId", element: lazyPage(<ProjectsPage />) },
      { path: "memory", element: lazyPage(<MemoryPage />) },
      { path: "insights", element: lazyPage(<InsightsPage />) },
      { path: "hooks", element: lazyPage(<HooksPage />) },
      { path: "web-access", element: lazyPage(<WebAccessPage />) },
      { path: "conversation-payloads/chats/:chatKey", element: lazyPage(<ConversationPayloadsPage />) },
    ].map((route) => ({ ...route, errorElement: <RouteError /> })),
  },
]);
