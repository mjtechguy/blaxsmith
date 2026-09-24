import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { useQueryClient } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { KeyRound } from "lucide-react";
import { accountError, changeMyPassword, mySessionsKey, passwordProblem, useAccount } from "../account";
import { TextField } from "../form-field";
import { GuardedSaveBar, useSaved } from "../layouts";

export const Route = createFileRoute("/me/settings/password")({ component: Password });

const blank = { currentPassword: "", newPassword: "", confirm: "" };

function Password() {
  const { scope, account } = useAccount();
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [saved, setSaved] = useSaved();
  const form = useForm({
    defaultValues: blank,
    onSubmit: async ({ value }) => {
      setError("");
      setNotice("");
      if (!value.currentPassword || passwordProblem(value.newPassword) || value.newPassword !== value.confirm) {
        setError(!value.currentPassword ? "Enter your current password." : passwordProblem(value.newPassword) ?? "The new passwords do not match.");
        return;
      }
      try {
        const response = await changeMyPassword(value.currentPassword, value.newPassword);
        form.reset(blank);
        await queryClient.invalidateQueries({ queryKey: mySessionsKey(scope) });
        setNotice(response.revokedSessions > 0n ? `Password changed. Your other ${response.revokedSessions === 1n ? "session was" : "sessions were"} signed out.` : "Password changed.");
        setSaved(true);
      } catch (cause) {
        setError(accountError(cause, "Your password could not be changed. Please try again."));
      }
    },
  });

  return <form className="settings-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
    <section className="editor-card" aria-labelledby="password-heading">
      <div className="editor-card-heading"><span className="project-symbol"><KeyRound size={18} aria-hidden="true" /></span><div><h2 id="password-heading">Change password</h2>
        <p>Use at least 12 characters. Changing it signs you out of every other session and cancels any open reset link.</p></div></div>
      <div className="editor-form">
        <input type="email" name="username" autoComplete="username" hidden readOnly value={account?.email ?? ""} />
        <form.Field name="currentPassword">
          {(field) => <TextField label="Current password" type="password" name={field.name} autoComplete="current-password" placeholder="Your current password"
            value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}
        </form.Field>
        <form.Field name="newPassword" validators={{ onBlur: ({ value }) => value ? passwordProblem(value) : undefined }}>
          {(field) => <TextField label="New password" type="password" name={field.name} autoComplete="new-password" placeholder="At least 12 characters"
            value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="confirm" validators={{ onChangeListenTo: ["newPassword"], onBlur: ({ value, fieldApi }) => value === fieldApi.form.getFieldValue("newPassword") ? undefined : "Passwords do not match." }}>
          {(field) => <TextField label="Confirm new password" type="password" name={field.name} autoComplete="new-password" placeholder="Enter it again"
            value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        {notice ? <p className="notice" role="status">{notice}</p> : null}
      </div>
    </section>
    <form.Subscribe selector={(state) => [state.isDirty, state.isSubmitting, state.canSubmit] as const}>
      {([dirty, submitting, canSubmit]) => <GuardedSaveBar dirty={dirty} saving={submitting} canSave={canSubmit} error={error} saved={saved}
        saveLabel="Change password" onCancel={() => { form.reset(blank); setError(""); }} />}
    </form.Subscribe>
  </form>;
}
