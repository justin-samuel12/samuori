import { useCallback, useState } from "react";
import { api, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Select } from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { useLoader } from "../hooks";
import { navigate } from "../router";
import { useCan } from "../session/session";

export function NewImagePage() {
  const can = useCan();
  const [projects, setProjects] = useState<Schemas["Project"][]>();
  const [choice, setChoice] = useState<string | null>(null);
  const load = useCallback(async () => {
    const { data } = await api.GET("/projects", {
      params: { query: { limit: 100 } },
    });
    setProjects(data?.items ?? []);
  }, []);
  useLoader(load);
  if (!projects) return <Loading />;
  const usable = projects.filter((p) =>
    can("createSpec", { teamId: p.teamId, projectId: p.id }),
  );
  return (
    <>
      <PageHeader
        help="new-image"
        title="New image"
        description="Choose the project the image belongs to."
      />
      {usable.length === 0 ? (
        <Alert tone="info">
          {projects.length === 0
            ? "You have no projects yet. A maintainer can create one on the Projects page."
            : "Your roles do not let you create images in any of your projects. A developer or maintainer role on a project's team does."}
        </Alert>
      ) : (
        <div className="samuori-form">
          <Select
            label="Project"
            options={usable.map((p) => ({ id: p.id, label: p.name }))}
            value={choice}
            onChange={setChoice}
          />
          <div className="samuori-actions samuori-actions--start">
            <Button
              isDisabled={!choice}
              onPress={() => navigate(`/projects/${choice}/new-image`)}
            >
              Continue
            </Button>
          </div>
        </div>
      )}
    </>
  );
}
