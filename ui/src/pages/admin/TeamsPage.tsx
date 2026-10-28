import { useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useTeams } from "../useTeams";
import { correctName, slug } from "../../format";

export function TeamsPage() {
  const { teams, reload } = useTeams();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    const { data, error } = await api.POST("/teams", {
      body: { name: slug(name) },
    });
    if (!data) {
      setError(describeError(error));
      return;
    }
    setOpen(false);
    setName("");
    setError(undefined);
    await reload();
  }

  return (
    <>
      <PageHeader
        help="people-and-access"
        title="Teams"
        description="Teams own projects. Grant people roles on a team from the Users page."
        actions={<Button onPress={() => setOpen(true)}>New team</Button>}
      />
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
          ]}
        />
      )}
      <Dialog title="New team" isOpen={open} onOpenChange={setOpen}>
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
            <Button variant="secondary" onPress={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit">Create team</Button>
          </div>
        </Form>
      </Dialog>
    </>
  );
}
