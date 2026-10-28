import { useQuery } from "@tanstack/react-query";
import {
  Autocomplete,
  Dialog,
  Header,
  Input,
  Menu,
  MenuItem,
  MenuSection,
  Modal,
  ModalOverlay,
  TextField,
  useFilter,
} from "react-aria-components";
import { BookOpen, FolderKanban, Search, type LucideIcon } from "lucide-react";
import { api } from "../api/client";
import { navigate } from "../router";
import { can } from "../session/can";
import type { Me } from "../session/session";
import { useHelpTopics } from "../help/topics";
import { accountNav, items, type Workspace } from "./nav";

interface Entry {
  id: string;
  label: string;
  detail?: string;
  icon: LucideIcon;
}

/** Jump to any page, project or guide the user may open (Ctrl+K or ⌘K). */
export function CommandPalette({
  me,
  list,
  isOpen,
  onOpenChange,
}: {
  me: Me;
  list: Workspace[];
  isOpen: boolean;
  onOpenChange: (v: boolean) => void;
}) {
  const { contains } = useFilter({ sensitivity: "base" });
  const seesProjects = can(me, "listProjects");
  const projects = useQuery({
    queryKey: ["projects", "palette"],
    queryFn: async () =>
      (await api.GET("/projects", { params: { query: { limit: 100 } } })).data
        ?.items ?? [],
    enabled: isOpen && seesProjects,
    staleTime: 60_000,
  });
  const help = useHelpTopics();

  const seen = new Set<string>();
  const pages: Entry[] = [];
  for (const w of list)
    for (const i of items(w))
      if (!seen.has(i.href)) {
        seen.add(i.href);
        pages.push({
          id: i.href,
          label: i.label,
          detail: w.label,
          icon: i.icon,
        });
      }
  for (const i of accountNav)
    if (!seen.has(i.href)) {
      seen.add(i.href);
      pages.push({
        id: i.href,
        label: i.label,
        detail: "Account",
        icon: i.icon,
      });
    }
  const sections: { title: string; entries: Entry[] }[] = [
    { title: "Go to", entries: pages },
    {
      title: "Projects",
      entries: (projects.data ?? []).map((p) => ({
        id: `/projects/${p.id}`,
        label: p.name,
        detail: "Project",
        icon: FolderKanban,
      })),
    },
    {
      title: "Help",
      entries: (help.data ?? []).map((t) => ({
        id: `/help/${t.id}`,
        label: t.title,
        detail: `Guide · ${t.section}`,
        icon: BookOpen,
      })),
    },
  ].filter((s) => s.entries.length > 0);

  return (
    <ModalOverlay
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      isDismissable
      className="samuori-overlay samuori-overlay--palette"
    >
      <Modal className="samuori-palette">
        <Dialog aria-label="Search and jump to" className="outline-none">
          <Autocomplete filter={contains}>
            <TextField
              aria-label="Search pages, projects and guides"
              autoFocus
              className="samuori-palette-search"
            >
              <Search size={18} aria-hidden="true" className="text-muted" />
              <Input
                placeholder="Search pages, projects and guides…"
                className="samuori-palette-input"
              />
              <kbd className="samuori-kbd">Esc</kbd>
            </TextField>
            <div
              className="samuori-palette-list"
              tabIndex={0}
              role="region"
              aria-label="Search results"
            >
              <Menu
                className="outline-none"
                aria-label="Results"
                onAction={(key) => {
                  onOpenChange(false);
                  navigate(String(key));
                }}
                renderEmptyState={() => (
                  <p className="m-0 px-4 py-8 text-center text-sm text-muted">
                    Nothing matches.
                  </p>
                )}
              >
                {sections.map((s) => (
                  <MenuSection
                    key={s.title}
                    className="samuori-palette-section"
                  >
                    <Header className="samuori-palette-header">
                      {s.title}
                    </Header>
                    {s.entries.map((e) => {
                      const Icon = e.icon;
                      return (
                        <MenuItem
                          key={e.id}
                          id={e.id}
                          textValue={`${e.label} ${e.detail ?? ""}`}
                          aria-label={
                            e.detail ? `${e.label}, ${e.detail}` : e.label
                          }
                          className="samuori-palette-item"
                        >
                          <Icon size={16} aria-hidden="true" />
                          <span className="min-w-0 flex-1 truncate">
                            {e.label}
                          </span>
                          {e.detail && (
                            <span className="truncate text-xs text-muted">
                              {e.detail}
                            </span>
                          )}
                        </MenuItem>
                      );
                    })}
                  </MenuSection>
                ))}
              </Menu>
            </div>
          </Autocomplete>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}
