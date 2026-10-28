import { useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useTeams, type Team } from "../useTeams";
import { correctName, slug } from "../../format";

export function TeamsPage() {
  const { teams, reload } = useTeams();
  const [open, setOpen] = useState(false);
  const [renaming, setRenaming] = useState<Team>();
  const [deleting, setDeleting] = useState<Team>();
  const [message, setMessage] = useState<string>();

  return (
    <>
      <PageHeader
        help="people-and-access"
        title="Teams"
        description="Teams own projects. Grant people roles on a team from the Users page."
        actions={<Button onPress={() => setOpen(true)}>New team</Button>}
      />
      {message && <Alert tone="success">{message}</Alert>}
      {!teams ? (
        <Loading />
      ) : (
        <Table
          label="Teams"
          rows={teams}
          empty="No teams yet. Create one to start adding projects."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (t) => t.name,
            },
            { id: "id", name: "ID", render: (t) => <code>{t.id}</code> },
            {
              id: "actions",
              name: "Actions",
              render: (t) => (
                <div className="samuori-actions">
                  <Button
                    variant="quiet"
                    onPress={() => setRenaming(t)}
                    aria-label={`Rename ${t.name}`}
                  >
                    Rename
                  </Button>
                  <Button
                    variant="quiet"
                    onPress={() => setDeleting(t)}
                    aria-label={`Delete ${t.name}`}
                  >
                    Delete
                  </Button>
                </div>
              ),
            },
          ]}
        />
      )}
      <TeamNameDialog
        isOpen={open}
        onClose={() => setOpen(false)}
        onSaved={async (t) => {
          setOpen(false);
          setMessage(`Team ${t.name} created.`);
          await reload();
        }}
      />
      {renaming && (
        <TeamNameDialog
          isOpen
          team={renaming}
          onClose={() => setRenaming(undefined)}
          onSaved={async (t) => {
            setRenaming(undefined);
            setMessage(`Team renamed to ${t.name}.`);
            await reload();
          }}
        />
      )}
      {deleting && (
        <DeleteTeamDialog
          team={deleting}
          onClose={() => setDeleting(undefined)}
          onDeleted={async () => {
            setMessage(`Team ${deleting.name} deleted.`);
            setDeleting(undefined);
            await reload();
          }}
        />
      )}
    </>
  );
}

function TeamNameDialog({
  isOpen,
  team,
  onClose,
  onSaved,
}: {
  isOpen: boolean;
  team?: Team;
  onClose: () => void;
  onSaved: (t: Team) => void | Promise<void>;
}) {
  const [name, setName] = useState(team?.name ?? "");
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    const body = { name: slug(name) };
    const { data, error } = team
      ? await api.PATCH("/teams/{teamId}", {
          params: { path: { teamId: team.id } },
          body,
        })
      : await api.POST("/teams", { body });
    if (!data) {
      setError(describeError(error));
      return;
    }
    setName("");
    setError(undefined);
    await onSaved(data);
  }

  return (
    <Dialog
      title={team ? `Rename ${team.name}` : "New team"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Team name"
          value={name}
          onChange={(v) => setName(correctName(v))}
          isRequired
          description="Saved in lowercase with dashes, for example payments."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{team ? "Rename team" : "Create team"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function DeleteTeamDialog({
  team,
  onClose,
  onDeleted,
}: {
  team: Team;
  onClose: () => void;
  onDeleted: () => void | Promise<void>;
}) {
  const [error, setError] = useState<string>();

  async function remove() {
    const { error } = await api.DELETE("/teams/{teamId}", {
      params: { path: { teamId: team.id } },
    });
    if (error) {
      setError(describeError(error));
      return;
    }
    await onDeleted();
  }

  return (
    <Dialog
      title={`Delete ${team.name}?`}
      size="confirm"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      {error && <Alert>{error}</Alert>}
      <p>
        Only a team with no projects (including archived and purged ones), role
        bindings, group mappings, notification rules or image sources can be
        deleted.
      </p>
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button variant="danger" onPress={() => void remove()}>
          Delete team
        </Button>
      </div>
    </Dialog>
  );
}
