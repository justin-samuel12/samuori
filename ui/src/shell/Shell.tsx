import { useEffect, useRef, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Button as AriaButton,
  Dialog as AriaDialog,
  Header,
  Heading,
  Link,
  Menu,
  MenuItem,
  MenuSection,
  MenuTrigger,
  Modal,
  ModalOverlay,
  Popover,
  Separator,
} from "react-aria-components";
import {
  Bell,
  Check,
  Info,
  LogOut,
  Menu as MenuIcon,
  Monitor,
  Moon,
  Search,
  Sun,
  X,
} from "lucide-react";
import { api, onServerUpdated, type Schemas } from "../api/client";
import { navigate, usePath } from "../router";
import { useSession, type Me } from "../session/session";
import { useHealth } from "./health";
import { BrandMark } from "../components/BrandMark";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import {
  accountNav,
  isCurrent,
  items,
  landing,
  locate,
  visibleWorkspaces,
  type Workspace,
} from "./nav";
import { CommandPalette } from "./CommandPalette";
import { AboutDialog } from "./About";
import { setTheme, useTheme, type Theme } from "./theme";

interface Props {
  me: Me;
  title: string;
  children: ReactNode;
}

const focusRing =
  "outline-none data-[focus-visible]:outline-2 data-[focus-visible]:outline-offset-2 data-[focus-visible]:outline-[var(--samuori-focus)]";

const iconButton = `inline-grid size-9 flex-none cursor-pointer place-items-center rounded-lg border-0 bg-transparent text-muted transition-colors hover:bg-raised hover:text-fg ${focusRing}`;

export function Shell({ me, title, children }: Props) {
  const path = usePath();
  const mainRef = useRef<HTMLElement>(null);
  const list = visibleWorkspaces(me);
  const { workspace, item } = locate(list, path);
  const [paletteOpen, setPaletteOpen] = useState(false);
  // The sheet belongs to the page it was opened on, so navigating closes it.
  const [sheetOn, setSheetOn] = useState<string | null>(null);

  useEffect(() => {
    document.title = `${title} · Samuori`;
    mainRef.current?.focus({ preventScroll: true });
  }, [title, path]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  return (
    <div className="samuori-app">
      <a href="#main" className="samuori-skip">
        Skip to content
      </a>
      <header className="samuori-topbar">
        <div className="samuori-topbar-inner">
          <AriaButton
            className={`${iconButton} -ml-1 xl:hidden`}
            aria-label="Open navigation"
            aria-expanded={sheetOn === path}
            onPress={() => setSheetOn(path)}
          >
            <MenuIcon size={20} aria-hidden="true" />
          </AriaButton>
          <Link
            href="/"
            className={`flex flex-none items-center gap-2.5 rounded-lg text-fg no-underline ${focusRing}`}
          >
            <BrandMark kind="logo" className="w-[120px]" />
          </Link>
          <nav
            aria-label="Main"
            className="ml-2 hidden min-w-0 flex-1 xl:block 2xl:ml-4"
          >
            <ul className="m-0 flex list-none items-center gap-0.5 p-0">
              {list.map((w) => (
                <li key={w.id}>
                  <WorkspaceLink w={w} current={w.id === workspace?.id} />
                </li>
              ))}
            </ul>
          </nav>
          <div className="ml-auto flex flex-none items-center gap-1.5 sm:gap-2">
            <AriaButton
              className={`hidden h-9 cursor-pointer items-center gap-2 rounded-lg border border-line bg-canvas pr-1.5 pl-3 text-sm text-muted transition-colors hover:border-line-strong hover:text-fg 2xl:inline-flex 2xl:w-64 ${focusRing}`}
              aria-label="Search"
              aria-keyshortcuts="Meta+K Control+K"
              onPress={() => setPaletteOpen(true)}
            >
              <Search size={15} aria-hidden="true" />
              <span className="flex-1 text-left">Search or jump to…</span>
              <kbd className="samuori-kbd">{shortcutLabel()}</kbd>
            </AriaButton>
            <AriaButton
              className={`${iconButton} 2xl:hidden`}
              aria-label="Search"
              aria-keyshortcuts="Meta+K Control+K"
              onPress={() => setPaletteOpen(true)}
            >
              <Search size={18} aria-hidden="true" />
            </AriaButton>
            <HealthPill />
            <NotificationsLink current={isCurrent("/notifications", path)} />
            <AccountMenu me={me} />
          </div>
        </div>
      </header>
      {workspace && <ContextBar workspace={workspace} path={path} />}
      <main
        className="samuori-main samuori-workspace"
        id="main"
        ref={mainRef}
        tabIndex={-1}
        data-workspace={workspace?.id}
        data-section={item?.href}
      >
        <UpdatedBanner />
        {children}
      </main>
      <NavSheet
        list={list}
        current={workspace?.id}
        path={path}
        isOpen={sheetOn === path}
        onOpenChange={(v) => setSheetOn(v ? path : null)}
      />
      <CommandPalette
        me={me}
        list={list}
        isOpen={paletteOpen}
        onOpenChange={setPaletteOpen}
      />
    </div>
  );
}

function shortcutLabel(): string {
  return /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘K" : "Ctrl K";
}

function WorkspaceLink({ w, current }: { w: Workspace; current: boolean }) {
  return (
    <Link
      href={landing(w)}
      aria-current={current ? "page" : undefined}
      className={`samuori-ws-link ${focusRing}`}
    >
      {w.label}
    </Link>
  );
}

/** The current workspace's name and its sections, so only the navigation that matters here is on screen. */
function ContextBar({
  workspace,
  path,
}: {
  workspace: Workspace;
  path: string;
}) {
  const Icon = workspace.icon;
  const all = items(workspace);
  const home = landing(workspace);
  const tabsRef = useRef<HTMLElement>(null);
  useEffect(() => {
    const strip = tabsRef.current;
    const tab = strip?.querySelector<HTMLElement>('[aria-current="page"]');
    if (!strip || !tab) return;
    const left = tab.offsetLeft - strip.offsetLeft;
    if (
      left < strip.scrollLeft ||
      left + tab.offsetWidth > strip.scrollLeft + strip.clientWidth
    )
      strip.scrollLeft = left - 24;
  }, [path]);
  if (all.length === 1 && path === home) return null;
  // A tab with the workspace's own name already says where the user is.
  const repeated =
    all.length > 1 && all.some((i) => i.label === workspace.label);
  const name = (
    <>
      <span
        className="grid size-7 place-items-center rounded-md bg-accent-soft text-info"
        aria-hidden="true"
      >
        <Icon size={15} />
      </span>
      {workspace.label}
    </>
  );
  return (
    <div className="samuori-contextbar">
      <div className="samuori-contextbar-inner">
        {repeated ? null : path === home ? (
          <p className="m-0 flex flex-none items-center gap-2 text-sm font-semibold text-fg">
            {name}
          </p>
        ) : (
          <Link
            href={home}
            className={`flex flex-none items-center gap-2 rounded-md text-sm font-semibold text-fg no-underline hover:text-info ${focusRing}`}
          >
            {name}
          </Link>
        )}
        {all.length > 1 && (
          <nav
            aria-label="Section"
            ref={tabsRef}
            className="samuori-tabs-scroll min-w-0 flex-1"
          >
            <ul className="m-0 flex list-none items-stretch gap-1 p-0">
              {workspace.groups.map((g, gi) => (
                <li key={gi} className="flex items-stretch gap-1">
                  {gi > 0 && (
                    <span
                      className="mx-1.5 my-auto h-4 w-px flex-none bg-line-strong"
                      aria-hidden="true"
                    />
                  )}
                  {g.label && (
                    <span className="my-auto px-1 text-[0.6875rem] font-semibold tracking-[0.08em] whitespace-nowrap text-muted uppercase">
                      {g.label}
                    </span>
                  )}
                  <ul className="m-0 flex list-none items-stretch gap-1 p-0">
                    {g.items.map((i) => {
                      const current =
                        locate([workspace], path).item?.href === i.href;
                      return (
                        <li key={i.href} className="flex">
                          <Link
                            href={i.href}
                            aria-current={current ? "page" : undefined}
                            className={`samuori-tab ${focusRing}`}
                          >
                            {i.label}
                          </Link>
                        </li>
                      );
                    })}
                  </ul>
                </li>
              ))}
            </ul>
          </nav>
        )}
      </div>
    </div>
  );
}

function NavSheet({
  list,
  current,
  path,
  isOpen,
  onOpenChange,
}: {
  list: Workspace[];
  current?: string;
  path: string;
  isOpen: boolean;
  onOpenChange: (v: boolean) => void;
}) {
  return (
    <ModalOverlay
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      isDismissable
      className="samuori-overlay samuori-overlay--sheet"
    >
      <Modal className="samuori-sheet">
        <AriaDialog
          aria-label="Navigation"
          className="flex h-full flex-col outline-none"
        >
          <div className="flex h-16 flex-none items-center gap-2.5 border-b border-line px-4">
            <BrandMark alt="" className="w-7 flex-none" />
            <Heading slot="title" className="m-0 text-base font-bold">
              Samuori
            </Heading>
            <AriaButton
              className={`${iconButton} ml-auto`}
              aria-label="Close navigation"
              onPress={() => onOpenChange(false)}
            >
              <X size={18} aria-hidden="true" />
            </AriaButton>
          </div>
          <nav aria-label="Workspaces" className="flex-1 overflow-y-auto p-3">
            {list.map((w) => {
              const Icon = w.icon;
              return (
                <section key={w.id} className="mb-3">
                  <h2
                    className={`m-0 flex items-center gap-2 px-2 py-1.5 text-xs font-semibold tracking-[0.06em] uppercase ${w.id === current ? "text-info" : "text-muted"}`}
                  >
                    <Icon size={14} aria-hidden="true" />
                    {w.label}
                  </h2>
                  <ul className="m-0 list-none p-0">
                    {items(w).map((i) => {
                      const on = locate([w], path).item?.href === i.href;
                      return (
                        <li key={i.href}>
                          <Link
                            href={i.href}
                            aria-current={on ? "page" : undefined}
                            className={`samuori-sheet-link ${focusRing}`}
                          >
                            {i.label}
                          </Link>
                        </li>
                      );
                    })}
                  </ul>
                </section>
              );
            })}
          </nav>
        </AriaDialog>
      </Modal>
    </ModalOverlay>
  );
}

function NotificationsLink({ current }: { current: boolean }) {
  const { data } = useQuery({
    queryKey: ["notifications", "unread"],
    queryFn: async () =>
      (
        await api.GET("/notifications", {
          params: { query: { unread: true, limit: 1 } },
        })
      ).data?.unread ?? 0,
    refetchInterval: 60_000,
  });
  const unread = data ?? 0;
  return (
    <Link
      href="/notifications"
      aria-current={current ? "page" : undefined}
      aria-label={
        unread > 0 ? `Notifications, ${unread} unread` : "Notifications"
      }
      className={`${iconButton} relative`}
    >
      <Bell size={18} aria-hidden="true" />
      {unread > 0 && (
        <span
          className="absolute top-1 right-1 grid min-w-4 place-items-center rounded-full bg-accent px-1 text-[0.625rem] leading-4 font-bold text-on-accent"
          aria-hidden="true"
        >
          {unread > 99 ? "99+" : unread}
        </span>
      )}
    </Link>
  );
}

const themes: { id: Theme; label: string; icon: typeof Sun }[] = [
  { id: "system", label: "System theme", icon: Monitor },
  { id: "light", label: "Light theme", icon: Sun },
  { id: "dark", label: "Dark theme", icon: Moon },
];

function AccountMenu({ me }: { me: Me }) {
  const { signOut } = useSession();
  const theme = useTheme();
  const [about, setAbout] = useState(false);
  const name = me.displayName ?? "Signed in";
  const role = me.roles[0]?.role;
  return (
    <>
      <MenuTrigger>
        <AriaButton
          aria-label={`Account menu for ${name}`}
          className={`grid size-9 flex-none cursor-pointer place-items-center rounded-full border border-line bg-raised p-0 text-xs font-bold text-fg transition-colors hover:border-line-strong ${focusRing}`}
        >
          {initials(name)}
        </AriaButton>
        <Popover
          placement="bottom end"
          offset={8}
          className="samuori-popover samuori-menu-popover"
        >
          <Menu
            className="samuori-menu"
            aria-label="Account"
            onAction={(key) => {
              const k = String(key);
              if (k === "sign-out") void signOut();
              else if (k === "about") setAbout(true);
              else if (k.startsWith("/")) navigate(k);
            }}
          >
            <MenuSection>
              <Header className="px-3 pt-2 pb-2">
                <span className="block truncate text-sm font-semibold text-fg">
                  {name}
                </span>
                {role && (
                  <span className="mt-0.5 block truncate text-[0.6875rem] tracking-wide text-muted uppercase">
                    {role.replace("-", " ")}
                  </span>
                )}
              </Header>
              {accountNav.map((i) => {
                const Icon = i.icon;
                return (
                  <MenuItem
                    key={i.href}
                    id={i.href}
                    className="samuori-menu-item"
                  >
                    <Icon size={16} aria-hidden="true" />
                    {i.label}
                  </MenuItem>
                );
              })}
            </MenuSection>
            <Separator className="samuori-menu-sep" />
            <MenuSection
              aria-label="Theme"
              selectionMode="single"
              selectedKeys={[theme]}
              disallowEmptySelection
              onSelectionChange={(keys) => {
                const [k] = [...(keys as Set<string>)];
                if (k) setTheme(k as Theme);
              }}
            >
              {themes.map((t) => {
                const Icon = t.icon;
                return (
                  <MenuItem key={t.id} id={t.id} className="samuori-menu-item">
                    {({ isSelected }) => (
                      <>
                        <Icon size={16} aria-hidden="true" />
                        <span className="flex-1">{t.label}</span>
                        {isSelected && <Check size={15} aria-hidden="true" />}
                      </>
                    )}
                  </MenuItem>
                );
              })}
            </MenuSection>
            <Separator className="samuori-menu-sep" />
            <MenuItem id="about" className="samuori-menu-item">
              <Info size={16} aria-hidden="true" />
              About Samuori
            </MenuItem>
            <MenuItem id="sign-out" className="samuori-menu-item">
              <LogOut size={16} aria-hidden="true" />
              Sign out
            </MenuItem>
          </Menu>
        </Popover>
      </MenuTrigger>
      <AboutDialog isOpen={about} onOpenChange={setAbout} />
    </>
  );
}

function initials(name: string): string {
  const parts = name.trim().split(/\s+/);
  return ((parts[0]?.[0] ?? "") + (parts[1]?.[0] ?? "")).toUpperCase() || "?";
}

const healthTone: Record<Schemas["Health"]["status"], string> = {
  ok: "bg-ok",
  degraded: "bg-warn",
  unavailable: "bg-bad",
};

const healthLabel: Record<Schemas["Health"]["status"], string> = {
  ok: "Healthy",
  degraded: "Degraded",
  unavailable: "Unavailable",
};

function HealthPill() {
  const { data } = useHealth();
  if (!data?.status) return null;
  return (
    <span
      className="inline-flex h-9 items-center gap-2 rounded-lg px-2 text-xs font-medium text-muted sm:border sm:border-line sm:px-3 xl:max-2xl:border-0 xl:max-2xl:px-2"
      title="Controller health: database, storage and messaging"
    >
      <span
        className={`size-2 flex-none rounded-full ${healthTone[data.status]}`}
        aria-hidden="true"
      />
      <span className="max-sm:sr-only xl:max-2xl:sr-only">
        {healthLabel[data.status]}
      </span>
    </span>
  );
}

/** Tells everyone signed in that Samuori was updated, in a dialog wherever they are; the banner stays as a reminder
 * after "Later". Reloading picks up the new portal; nobody is signed out. */
function UpdatedBanner() {
  const [version, setVersion] = useState<string>();
  const [asking, setAsking] = useState(false);
  useEffect(
    () =>
      onServerUpdated((v) => {
        setVersion(v);
        setAsking(true);
      }),
    [],
  );
  if (!version) return null;
  return (
    <>
      <Dialog
        title="Samuori was updated"
        size="confirm"
        isOpen={asking}
        onOpenChange={setAsking}
      >
        <p>
          Version {version} is running. Reload to use it; you stay signed in.
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setAsking(false)}>
            Later
          </Button>
          <Button onPress={() => window.location.reload()}>Reload now</Button>
        </div>
      </Dialog>
      <div className="samuori-alert samuori-alert--info mb-6" role="status">
        <div className="samuori-alert-body">
          Samuori was updated to {version}. Reload to use the new version; you
          stay signed in.{" "}
          <Button variant="quiet" onPress={() => window.location.reload()}>
            Reload
          </Button>
        </div>
      </div>
    </>
  );
}
