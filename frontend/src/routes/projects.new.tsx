import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { FolderPlus, RefreshCw } from "lucide-react";
import { TextField } from "../form-field";
import { CreateFlow, SummaryList } from "../layouts";
import { useScope } from "../workspace-ui";
import { createProject } from "../workflow";
import { projectFlowSteps } from "../project-settings";

export const Route = createFileRoute("/projects/new")({ component: NewProject });
const slugPattern = /^[a-z][a-z0-9-]{2,63}$/;
const slugFrom = (name: string) => name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^[^a-z]+/, "").replace(/-+$/, "").slice(0, 64);

function NewProject() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { org } = useScope();
  const [error, setError] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
  const form = useForm({
    defaultValues: { name: "", slug: "" },
    onSubmit: async ({ value }) => {
      setError("");
      if (value.name.trim().length < 1 || value.name.trim().length > 160 || !slugPattern.test(value.slug.trim())) {
        setError("Check the project name and URL name before creating it.");
        return;
      }
      try {
        const response = await createProject(value.slug.trim(), value.name.trim());
        await queryClient.invalidateQueries({ queryKey: ["projects", org] });
        if (response.project?.id) await navigate({ to: "/projects/$projectId/setup", params: { projectId: response.project.id }, search: { step: "source" } as never });
        else setError("The project was created, but its page could not be opened. Open it from Projects.");
      } catch (cause) {
        setError(ConnectError.from(cause).code === Code.AlreadyExists ? "That URL name is already in use." : "The project could not be created. Please try again.");
      }
    },
  });

  return <CreateFlow title="New project" description="A project keeps a repository, its checks, recipes, access, and runs together. Set it up in five short steps; you can skip any step and return from Settings."
    back={{ href: "/projects", label: "Projects" }} steps={projectFlowSteps("details")}
    summary={<form.Subscribe selector={(state) => state.values}>{(values) => <>
      <h2>Summary</h2>
      <SummaryList items={[
        { label: "Name", value: values.name.trim() || "Not set", done: Boolean(values.name.trim()) },
        { label: "URL name", value: values.slug.trim() ? <code>{values.slug.trim()}</code> : "Not set", done: slugPattern.test(values.slug.trim()) },
        { label: "Source", value: "Next step" }, { label: "Verification", value: "After source" }, { label: "Recipe", value: "Optional" }, { label: "Access", value: "Optional" },
      ]} />
      <p className="form-hint">Runs use files already committed to Git. A project does not create a persistent pod or terminal.</p>
    </>}</form.Subscribe>}>
    <section className="editor-card" aria-labelledby="project-details-heading">
      <div className="editor-card-heading"><span className="project-symbol"><FolderPlus size={18} aria-hidden="true" /></span><div><h2 id="project-details-heading">Details</h2><p>How this project appears to your organization.</p></div></div>
      <form className="editor-form" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <form.Field name="name" validators={{ onBlur: ({ value }) => value.trim().length >= 1 && value.trim().length <= 160 ? undefined : "Use 1–160 characters." }}>
          {(field) => <TextField autoFocus label="Project name" name={field.name} autoComplete="off" placeholder="Customer portal" value={field.state.value}
            onChange={(value) => { field.handleChange(value); if (!slugTouched) form.setFieldValue("slug", slugFrom(value)); }} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="slug" validators={{ onBlur: ({ value }) => slugPattern.test(value.trim()) ? undefined : "Use 3–64 lowercase letters, numbers, or hyphens; start with a letter." }}>
          {(field) => <TextField label="URL name" name={field.name} autoComplete="off" placeholder="customer-portal" value={field.state.value}
            onChange={(value) => { setSlugTouched(true); field.handleChange(value); }} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions"><Link to="/projects" className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <FolderPlus size={15} aria-hidden="true" />}{submitting ? "Creating…" : "Create and continue"}</button>}
        </form.Subscribe></div>
      </form>
    </section>
  </CreateFlow>;
}
