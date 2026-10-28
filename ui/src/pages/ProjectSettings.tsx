import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { Checkbox, TextField } from "../components/Field";
import { Alert } from "../components/Feedback";
import { correctName } from "../format";
import { useCan } from "../session/session";
import type { Scope } from "../session/can";

type Project = Schemas["Project"];

/** Renames the project within its team; a project never moves to another team. */
export function RenameProjectButton({
  project,
  scope,
  onChange,
}: {
  project: Project;
  scope: Scope;
  onChange: (p: Project) => void;
}) {
  const can = useCan();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState(project.name);
  const [error, setError] = useState<string>();
  if (!can("updateProject", scope)) return null;
  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const { data, error } = await api.PATCH("/projects/{projectId}", {
      params: { path: { projectId: project.id } },
      body: { name: correctName(name, { done: true }) },
    });
    if (!data) {
      setError(describeError(error, "The project could not be renamed."));
      return;
    }
    setOpen(false);
    onChange(data);
  }
  return (
    <>
      <Button
        variant="secondary"
        onPress={() => {
          setName(project.name);
          setError(undefined);
          setOpen(true);
        }}
      >
        Rename
      </Button>
      <Dialog title="Rename project" isOpen={open} onOpenChange={setOpen}>
        <Form onSubmit={submit} className="samuori-form">
          {error && <Alert>{error}</Alert>}
          <TextField
            label="Name"
            value={name}
            onChange={(v) => setName(correctName(v))}
            isRequired
            description="Lower-case letters, digits and dashes, unique within the team. Builds, images and history keep their IDs; image tags and repositories already published are not renamed."
          />
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit">Rename</Button>
          </div>
        </Form>
      </Dialog>
    </>
  );
}

/**
 * The locations the project's builds may run in (data residency). Hidden while the organization has one location
 * and the project names none; only administrators change it.
 */
export function ProjectLocations({
  projectId,
  scope,
}: {
  projectId: string;
  scope: Scope;
}) {
  const can = useCan();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [chosen, setChosen] = useState<string[]>([]);
  const [error, setError] = useState<string>();
  const all = useQuery({
    queryKey: ["locations"],
    queryFn: async () => (await api.GET("/locations")).data?.items ?? [],
    enabled: can("listLocations"),
  });
  const current = useQuery({
    queryKey: ["project-locations", projectId],
    queryFn: async () => {
      const { data } = await api.GET("/projects/{projectId}/locations", {
        params: { path: { projectId } },
      });
      if (!data) throw new Error("the project's locations could not be read");
      return data.locations;
    },
    enabled: can("getProjectLocations", scope),
  });
  const locations = all.data ?? [];
  const allowed = current.data;
  if (!allowed || (locations.length < 2 && allowed.length === 0)) return null;
  async function save() {
    setError(undefined);
    const { data, error } = await api.PUT("/projects/{projectId}/locations", {
      params: { path: { projectId } },
      body: { locations: chosen },
    });
    if (!data) {
      setError(describeError(error, "The locations could not be saved."));
      return;
    }
    queryClient.setQueryData(["project-locations", projectId], data.locations);
    setOpen(false);
  }
  return (
    <>
      <dt>Build locations</dt>
      <dd>
        {allowed.length ? allowed.join(", ") : "Any location"}
        {can("setProjectLocations", scope) && (
          <div className="samuori-inline">
            <Button
              variant="quiet"
              onPress={() => {
                setChosen(allowed);
                setError(undefined);
                setOpen(true);
              }}
            >
              Change locations
            </Button>
          </div>
        )}
        <Dialog title="Build locations" isOpen={open} onOpenChange={setOpen}>
          {error && <Alert>{error}</Alert>}
          <p className="mt-0">
            Builds of this project run only on worker pools in the chosen
            locations. Choose none to allow any location.
          </p>
          {locations.map((l) => (
            <Checkbox
              key={l.name}
              isSelected={chosen.includes(l.name)}
              onChange={(on) =>
                setChosen(
                  on ? [...chosen, l.name] : chosen.filter((x) => x !== l.name),
                )
              }
            >
              {l.name}
              {l.description && (
                <span className="samuori-muted"> — {l.description}</span>
              )}
            </Checkbox>
          ))}
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => setOpen(false)}>
              Cancel
            </Button>
            <Button onPress={() => void save()}>Save locations</Button>
          </div>
        </Dialog>
      </dd>
    </>
  );
}
