import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { UserRound } from "lucide-react";
import { accountError, emailProblem, getMyProfile, profileKey, updateMyProfile, useAccount } from "../account";
import { TextField } from "../form-field";
import type { MyProfile } from "../gen/blaxsmith/api/v1/account_pb";
import { GuardedSaveBar, SummaryList, useSaved } from "../layouts";
import { Card, CopyValue, StatePanel } from "../ui";

export const Route = createFileRoute("/me/settings/profile")({ component: Profile });

const roleLabel: Record<string, string> = { owner: "Owner", admin: "Admin", member: "Member", viewer: "Viewer" };

function Profile() {
  const { scope } = useAccount();
  const profile = useQuery({ queryKey: profileKey(scope), enabled: Boolean(scope), queryFn: ({ signal }) => getMyProfile(signal), staleTime: 60_000 });
  if (profile.isPending) return <StatePanel kind="loading" title="Loading your profile" />;
  if (profile.isError || !profile.data) return <StatePanel kind="error" title="Your profile is unavailable" retry={() => void profile.refetch()} />;
  return <ProfileEditor profile={profile.data} scope={scope} />;
}

function ProfileEditor({ profile, scope }: { profile: MyProfile; scope: string }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [saved, setSaved] = useSaved();
  const form = useForm({
    defaultValues: { displayName: profile.displayName, email: profile.email, currentPassword: "" },
    onSubmit: async ({ value }) => {
      setError("");
      setNotice("");
      const displayName = value.displayName.trim();
      const email = value.email.trim().toLowerCase();
      const emailChanged = email !== profile.email;
      if (displayName.length > 160 || (emailChanged && emailProblem(email)) || (emailChanged && !value.currentPassword)) {
        setError(emailChanged && !value.currentPassword ? "Enter your current password to change your email." : "Check the highlighted fields.");
        return;
      }
      try {
        const response = await updateMyProfile({
          displayName: displayName !== profile.displayName ? displayName : undefined,
          email: emailChanged ? email : undefined,
          currentPassword: emailChanged ? value.currentPassword : undefined,
        });
        const next = response.profile ?? profile;
        queryClient.setQueryData(profileKey(scope), next);
        form.reset({ displayName: next.displayName, email: next.email, currentPassword: "" });
        if (Number(response.revokedSessions) > 0) setNotice(`Your other ${response.revokedSessions === 1n ? "session was" : "sessions were"} signed out.`);
        setSaved(true);
      } catch (cause) {
        setError(accountError(cause));
      }
    },
  });

  return <>
    <form className="settings-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <section className="editor-card" aria-labelledby="profile-heading">
        <div className="editor-card-heading"><span className="project-symbol"><UserRound size={18} aria-hidden="true" /></span><div><h2 id="profile-heading">Profile</h2>
          <p>How you appear to your organization and the email you sign in with.</p></div></div>
        <div className="editor-form">
          <form.Field name="displayName" validators={{ onBlur: ({ value }) => value.trim().length <= 160 ? undefined : "Use at most 160 characters." }}>
            {(field) => <TextField label="Display name" name={field.name} autoComplete="name" placeholder="Jordan Lee" required={false}
              value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
          </form.Field>
          <form.Field name="email" validators={{ onBlur: ({ value }) => emailProblem(value) }}>
            {(field) => <>
              <TextField label="Email" type="email" name={field.name} autoComplete="email" placeholder="you@example.com"
                value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />
              <small className="form-hint">{profile.emailVerified ? "Verified." : "Not verified yet: no mail is sent."} Changing it signs you out everywhere else.</small>
            </>}
          </form.Field>
          <form.Subscribe selector={(state) => state.values.email.trim().toLowerCase() !== profile.email}>
            {(emailChanged) => emailChanged ? <form.Field name="currentPassword">
              {(field) => <TextField label="Current password" type="password" name={field.name} autoComplete="current-password" placeholder="Needed to change your email"
                value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}
            </form.Field> : null}
          </form.Subscribe>
          {notice ? <p className="notice" role="status">{notice}</p> : null}
        </div>
      </section>
      <form.Subscribe selector={(state) => [state.values.displayName !== profile.displayName || state.values.email.trim().toLowerCase() !== profile.email, state.isSubmitting, state.canSubmit] as const}>
        {([dirty, submitting, canSubmit]) => <GuardedSaveBar dirty={dirty} saving={submitting} canSave={canSubmit} error={error} saved={saved}
          onCancel={() => { form.reset(); setError(""); }} />}
      </form.Subscribe>
    </form>
    <Card title="Account" description="Set by your organization.">
      <SummaryList items={[
        { label: "Organization", value: profile.organizationName },
        { label: "Role", value: roleLabel[profile.role] ?? profile.role },
        { label: "Account ID", value: <CopyValue value={profile.principalId} label="Account ID" chars={13} /> },
      ]} />
    </Card>
  </>;
}
