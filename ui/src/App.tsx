import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Link, RouterProvider } from "react-aria-components";
import { Button } from "./components/Button";
import { Alert, PageHeader, PageSpinner } from "./components/Feedback";
import { AccountPage } from "./pages/AccountPage";
import { ApiReferencePage } from "./help/ApiReferencePage";
import { HelpPage } from "./help/HelpPage";
import { HomePage } from "./pages/HomePage";
import { LoginPage } from "./pages/LoginPage";
import { ProjectPage } from "./pages/ProjectPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { BuildPage } from "./pages/BuildPage";
import { BuildsPage } from "./pages/BuildsPage";
import { NewImagePage } from "./pages/NewImagePage";
import { NotificationsPage } from "./pages/NotificationsPage";
import { PipelinesPage } from "./pages/PipelinesPage";
import { NotificationChannelsPage } from "./pages/admin/NotificationChannelsPage";
import { SetupPage } from "./pages/SetupPage";
import { QueuePage } from "./pages/admin/QueuePage";
import { BuildPoliciesPage } from "./pages/admin/BuildPolicies";
import {
  EgressPage,
  EnginesPage,
  MessagingPage,
  RecipesPage,
  StoragePage,
} from "./pages/admin/SystemPages";
import { WorkersPage } from "./pages/admin/WorkersPage";
import { ServersPage } from "./pages/admin/ServersPage";
import { ProvidersPage } from "./pages/admin/ProvidersPage";
import { RegistriesPage } from "./pages/admin/RegistriesPage";
import { CertificatesPage } from "./pages/admin/CertificatesPage";
import { QuarantinesPage } from "./pages/admin/QuarantinesPage";
import { ImagePage, ImagesPage } from "./pages/ImagesPage";
import { ApprovalsPage } from "./pages/ApprovalsPage";
import { SecretsPage } from "./pages/admin/SecretsPage";
import { SnapshotPage } from "./pages/SnapshotPage";
import { UploadPage } from "./pages/UploadPage";
import { WizardPage } from "./wizard/WizardPage";
import {
  KubeConversionPage,
  KubeConversionsPage,
} from "./kube/KubeConversionPage";
import { KubeWizardPage } from "./kube/KubeWizardPage";
import { KubeWorkspacePage } from "./kube/KubeWorkspacePage";
import { IdentityPage } from "./pages/admin/IdentityPage";
import { ObservabilityPage } from "./pages/admin/ObservabilityPage";
import { BackupsPage } from "./pages/admin/BackupsPage";
import { ConfigurationPage } from "./pages/admin/ConfigurationPage";
import { UpgradesPage } from "./pages/admin/UpgradesPage";
import { CapacityPage } from "./pages/admin/CapacityPage";
import { DatabasePage } from "./pages/admin/DatabasePage";
import { FederationPage } from "./pages/admin/FederationPage";
import { OverviewPage } from "./pages/admin/OverviewPage";
import { TeamsPage } from "./pages/admin/TeamsPage";
import { UsersPage } from "./pages/admin/UsersPage";
import { match, navigate, usePath } from "./router";
import { Shell } from "./shell/Shell";
import { isOperator, isReviewer } from "./shell/nav";
import {
  isOrgAdmin,
  SessionProvider,
  useMe,
  useSession,
  type Me,
} from "./session/session";
import "./app.css";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: false } },
});

interface Route {
  path: string;
  title: string;
  render: (p: Record<string, string>) => ReactNode;
  admin?: boolean;
  operator?: boolean;
  reviewer?: boolean;
}

const routes: Route[] = [
  { path: "/", title: "Overview", render: () => <HomePage /> },
  { path: "/projects", title: "Projects", render: () => <ProjectsPage /> },
  {
    path: "/projects/:id",
    title: "Project",
    render: (p) => <ProjectPage id={p.id ?? ""} />,
  },
  {
    path: "/projects/:id/new-image",
    title: "New image",
    render: (p) => <WizardPage projectId={p.id ?? ""} />,
  },
  {
    path: "/projects/:id/kubernetes",
    title: "Kubernetes configuration",
    render: (p) => <KubeConversionsPage projectId={p.id ?? ""} />,
  },
  {
    path: "/projects/:id/kubernetes/new",
    title: "Generate Kubernetes configuration",
    render: (p) => <KubeWizardPage projectId={p.id ?? ""} />,
  },
  {
    path: "/kubernetes",
    title: "Kubernetes",
    render: () => <KubeWorkspacePage />,
  },
  {
    path: "/kube-conversions/:id",
    title: "Kubernetes configuration",
    render: (p) => <KubeConversionPage id={p.id ?? ""} />,
  },
  {
    path: "/projects/:id/upload",
    title: "Upload source",
    render: (p) => <UploadPage projectId={p.id ?? ""} />,
  },
  {
    path: "/projects/:id/snapshots/:sid",
    title: "Uploaded source",
    render: (p) => (
      <SnapshotPage projectId={p.id ?? ""} snapshotId={p.sid ?? ""} />
    ),
  },
  { path: "/account", title: "Account", render: () => <AccountPage /> },
  { path: "/help", title: "Help", render: () => <HelpPage /> },
  {
    path: "/help/api-reference",
    title: "API reference",
    render: () => <ApiReferencePage />,
  },
  {
    path: "/help/:topic",
    title: "Help",
    render: (p) => <HelpPage topic={p.topic ?? ""} />,
  },
  {
    path: "/notifications",
    title: "Notifications",
    render: () => <NotificationsPage />,
  },
  {
    path: "/admin",
    title: "Administration",
    render: () => <OverviewPage />,
    admin: true,
  },
  {
    path: "/admin/teams",
    title: "Teams",
    render: () => <TeamsPage />,
    admin: true,
  },
  {
    path: "/admin/users",
    title: "Users",
    render: () => <UsersPage />,
    admin: true,
  },
  {
    path: "/admin/identity",
    title: "Identity",
    render: () => <IdentityPage />,
    admin: true,
  },
  { path: "/builds", title: "Build runs", render: () => <BuildsPage /> },
  {
    path: "/builds/:id",
    title: "Build",
    render: (p) => <BuildPage id={p.id ?? ""} />,
  },
  { path: "/new-image", title: "New image", render: () => <NewImagePage /> },
  { path: "/images", title: "Images", render: () => <ImagesPage /> },
  {
    path: "/images/:id",
    title: "Image",
    render: (p) => <ImagePage id={p.id ?? ""} />,
  },
  { path: "/approvals", title: "Approvals", render: () => <ApprovalsPage /> },
  { path: "/pipelines", title: "Pipelines", render: () => <PipelinesPage /> },
  {
    path: "/admin/registries",
    title: "Registries",
    render: () => <RegistriesPage />,
    admin: true,
  },
  {
    path: "/admin/certificates",
    title: "Certificates",
    render: () => <CertificatesPage />,
    admin: true,
  },
  {
    path: "/admin/quarantines",
    title: "Quarantined images",
    render: () => <QuarantinesPage />,
    admin: true,
  },
  {
    path: "/admin/queue",
    title: "Build queue",
    render: () => <QueuePage />,
    operator: true,
  },
  {
    path: "/admin/servers",
    title: "Servers",
    render: () => <ServersPage />,
    admin: true,
  },
  {
    path: "/admin/workers",
    title: "Nodes & workers",
    render: () => <WorkersPage />,
    admin: true,
  },
  {
    path: "/admin/capacity",
    title: "Capacity & activity",
    render: () => <CapacityPage />,
    operator: true,
  },
  {
    path: "/admin/engines",
    title: "Build engines",
    render: () => <EnginesPage />,
    operator: true,
  },
  {
    path: "/admin/secrets",
    title: "Secrets",
    render: () => <SecretsPage />,
    admin: true,
  },
  {
    path: "/admin/egress",
    title: "Network egress",
    render: () => <EgressPage />,
    admin: true,
  },
  {
    path: "/admin/providers",
    title: "Build & conversion providers",
    render: () => <ProvidersPage />,
    admin: true,
  },
  {
    path: "/admin/recipes",
    title: "Recipes & base images",
    render: () => <RecipesPage />,
    admin: true,
  },
  {
    path: "/admin/policies",
    title: "Build policies",
    render: () => <BuildPoliciesPage />,
    admin: true,
  },
  {
    path: "/admin/storage",
    title: "Artifact storage",
    render: () => <StoragePage />,
    operator: true,
  },
  {
    path: "/admin/messaging",
    title: "Messaging",
    render: () => <MessagingPage />,
    operator: true,
  },
  {
    path: "/admin/notifications",
    title: "Notification channels",
    render: () => <NotificationChannelsPage />,
    admin: true,
  },
  {
    path: "/admin/federation",
    title: "Federation",
    render: () => <FederationPage />,
    admin: true,
  },
  {
    path: "/admin/upgrades",
    title: "Upgrades & Diagnostics",
    render: () => <UpgradesPage />,
    operator: true,
  },
  {
    path: "/admin/database",
    title: "Database",
    render: () => <DatabasePage />,
    admin: true,
  },
  {
    path: "/admin/backups",
    title: "Backup & Restore",
    render: () => <BackupsPage />,
    admin: true,
  },
  {
    path: "/admin/configuration",
    title: "Export & import",
    render: () => <ConfigurationPage />,
    admin: true,
  },
  {
    path: "/admin/observability",
    title: "Observability & Audit",
    render: () => <ObservabilityPage />,
    operator: true,
    reviewer: true,
  },
];

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider navigate={navigate}>
        <SessionProvider>
          <Root />
        </SessionProvider>
      </RouterProvider>
    </QueryClientProvider>
  );
}

function Root() {
  const { state, refresh } = useSession();
  switch (state.kind) {
    case "loading":
      return (
        <PageSpinner
          label={state.reconnecting ? "Reconnecting…" : "Loading"}
          showLabel={state.reconnecting}
        />
      );
    case "unreachable":
      return (
        <main className="samuori-auth-page" id="main">
          <div className="samuori-auth-card">
            <h1>Cannot reach the server</h1>
            <Alert>
              The Samuori server did not respond. Check that it is running, then
              try again.
            </Alert>
            <Button onPress={() => void refresh()}>Try again</Button>
          </div>
        </main>
      );
    case "setup":
      return <SetupPage />;
    case "anonymous":
      return <LoginPage notice={state.notice} />;
    case "signed-in":
      return <SignedIn me={state.me} />;
  }
}

function resolve(
  path: string,
  me: Me,
): { route?: Route; params: Record<string, string> } {
  for (const r of routes) {
    const params = match(r.path, path);
    const allowed =
      r.operator || r.reviewer
        ? isOrgAdmin(me) ||
          (r.operator === true && isOperator(me)) ||
          (r.reviewer === true && isReviewer(me))
        : !r.admin || isOrgAdmin(me);
    if (params && allowed) return { route: r, params };
  }
  return { params: {} };
}

function SignedIn({ me }: { me: Me }) {
  const path = usePath();
  const { route, params } = resolve(path, me);
  return (
    <Shell me={me} title={route?.title ?? "Not found"}>
      <AuthenticatedRoute>
        {route ? route.render(params) : <NotFound />}
      </AuthenticatedRoute>
    </Shell>
  );
}

function AuthenticatedRoute({ children }: { children: ReactNode }) {
  useMe();
  return <>{children}</>;
}

function NotFound() {
  return (
    <>
      <PageHeader title="Page not found" />
      <p>This page does not exist, or you do not have access to it.</p>
      <Link href="/" className="samuori-link">
        Go to Overview
      </Link>
    </>
  );
}
