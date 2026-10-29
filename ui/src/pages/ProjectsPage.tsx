import { useCallback, useState, type FormEvent } from "react";
import { useLoader } from "../hooks";
import { Form, Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { Checkbox, RadioGroup, Select, TextField } from "../components/Field";
import {
  Alert,
  Badge,
  EmptyState,
  Loading,
  PageHeader,
} from "../components/Feedback";
import { Table } from "../components/Table";
import { navigate, takeNotice } from "../router";
import { useCan } from "../session/session";
import { teamName, useTeams, type Team } from "./useTeams";
import { correctName, slug } from "../format";

type Project = Schemas["Project"];

export const publicationModes = [
  {
    id: "manual",
    label: "Manual",
    description:
      "Images are published only when someone with permission chooses to.",
  },
  {
    id: "automatic",
    label: "Automatic",
    description:
      "Images that pass every required check are published when policy allows.",
  },
];

export function ProjectsPage() {
  const { teams } = useTeams();
  const [items, setItems] = useState<Project[]>();
  const [cursor, setCursor] = useState<string | null>(null);
  const [error, setError] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [showArchived, setShowArchived] = useState(false);
  const [notice] = useState(takeNotice);
  const can = useCan();
  // Creating a project needs maintainer or administrator on its team or the organization, not on another project.
  const creatable = (teams ?? []).filter((t) =>
    can("createProject", { teamId: t.id }),
  );
  const canCreate = creatable.length > 0;

  const load = useCallback(
    async (after?: string) => {
      const { data, error, response } = await api.GET("/projects", {
        params: {
          query: {
            limit: 50,
            cursor: after,
            include: showArchived ? "archived" : undefined,
          },
        },
      });
      if (response.status === 403) {
        setItems([]);
        return;
      }
      if (!data) {
        setError(describeError(error, "Projects could not be loaded."));
        return;
      }
      setItems((prev) =>
        after ? [...(prev ?? []), ...data.items] : data.items,
      );
      setCursor(data.nextCursor ?? null);
    },
    [showArchived],
  );

  useLoader(load);

  return (
    <>
      <PageHeader
        help="projects"
        title="Projects"
        description="A project groups the images built from one application."
        actions={
          canCreate && (
            <Button onPress={() => setCreating(true)}>New project</Button>
          )
        }
      />
      <Checkbox isSelected={showArchived} onChange={setShowArchived}>
        Show archived projects
      </Checkbox>
      {notice && <Alert tone="success">{notice}</Alert>}
      {error && <Alert>{error}</Alert>}
      {!items && !error && <Loading />}
      {items && (
        <Table
          label="Projects"
          rows={items}
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (p) => (
                <span className="inline-flex items-center gap-2">
                  <Link href={`/projects/${p.id}`} className="samuori-link">
                    {p.name}
                  </Link>
                  {p.state === "archived" && (
                    <Badge tone="warn">Archived</Badge>
                  )}
                  {p.state === "purged" && <Badge tone="bad">Purged</Badge>}
                </span>
              ),
            },
            {
              id: "team",
              name: "Team",
              render: (p) => p.teamName ?? teamName(teams, p.teamId),
            },
            {
              id: "mode",
              name: "Publishing",
              render: (p) =>
                p.publicationMode === "automatic" ? "Automatic" : "Manual",
            },
            {
              id: "created",
              name: "Created",
              render: (p) => new Date(p.createdAt).toLocaleDateString(),
            },
          ]}
          empty={
            <EmptyState title="No projects yet">
              <p>
                {canCreate
                  ? "Create a project to start building images."
                  : "You will see projects here once a maintainer adds you to a team."}
              </p>
            </EmptyState>
          }
        />
      )}
      {cursor && (
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => void load(cursor)}>
            Load more
          </Button>
        </div>
      )}
      <CreateProjectDialog
        isOpen={creating}
        onOpenChange={setCreating}
        teams={creatable}
        onCreated={(p) => {
          setCreating(false);
          navigate(`/projects/${p.id}`);
        }}
      />
    </>
  );
}

function CreateProjectDialog({
  isOpen,
  onOpenChange,
  teams,
  onCreated,
}: {
  isOpen: boolean;
  onOpenChange: (o: boolean) => void;
  teams: Team[];
  onCreated: (p: Project) => void;
}) {
  const [name, setName] = useState("");
  const [teamId, setTeamId] = useState<string | null>(null);
  const [mode, setMode] = useState("manual");
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!teamId) {
      setError("Choose a team.");
      return;
    }
    const { data, error } = await api.POST("/projects", {
      body: {
        name: slug(name),
        teamId,
        publicationMode: mode as "manual" | "automatic",
      },
    });
    if (data) onCreated(data);
    else setError(describeError(error));
  }

  return (
    <Dialog title="New project" isOpen={isOpen} onOpenChange={onOpenChange}>
      {teams.length === 0 ? (
        <Alert tone="warning">
          You are not a member of any team. An administrator must create a team
          and add you first.
        </Alert>
      ) : (
        <Form onSubmit={submit} className="samuori-form">
          {error && <Alert>{error}</Alert>}
          <TextField
            placeholder="web"
            label="Project name"
            value={name}
            onChange={(v) => setName(correctName(v))}
            isRequired
            description="Saved in lowercase with dashes, for example web-api."
          />
          <Select
            label="Team"
            placeholder="Choose a team"
            options={teams.map((t) => ({ id: t.id, label: t.name }))}
            value={teamId}
            onChange={setTeamId}
            isRequired
          />
          <RadioGroup
            label="Publishing"
            value={mode}
            onChange={setMode}
            options={publicationModes}
          />
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit">Create project</Button>
          </div>
        </Form>
      )}
    </Dialog>
  );
}
