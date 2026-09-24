import { useId, useState } from "react";
import { Check, Copy } from "lucide-react";
import type { AccountLink } from "./gen/blaxsmith/api/v1/users_pb";
import { setupUrl } from "./users";

// Shows a freshly issued setup/reset link once. It lives only in component
// state: leaving the page discards it, and the server keeps only a digest.
export function OneTimeLink({ link, username }: { link: AccountLink; username: string }) {
  const id = useId();
  const url = setupUrl(link.token);
  const [copied, setCopied] = useState<"" | "done" | "failed">("");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(url);
      setCopied("done");
    } catch {
      setCopied("failed");
    }
  };
  return <div className="editor-form">
    <p className="notice"><strong>Copy this link now.</strong> It is shown only once. Send it to <strong>{username}</strong> over a channel you trust; it lets them {link.purpose === "reset" ? "set a new password" : "choose a password and sign in"}. It works once and expires {new Date(link.expiresAt).toLocaleString()}.</p>
    <div className="form-field"><label htmlFor={id}>{link.purpose === "reset" ? "Password reset link" : "Account setup link"}</label>
      <span className="field-with-action"><input id={id} readOnly value={url} className="mono" onFocus={(event) => event.currentTarget.select()} />
        <button type="button" className="field-action" onClick={() => void copy()} aria-label="Copy link">{copied === "done" ? <Check size={17} aria-hidden="true" /> : <Copy size={17} aria-hidden="true" />}</button></span>
      <span role="status" className={copied === "failed" ? "form-field-error" : "sr-only"}>{copied === "failed" ? "Copy failed. Select the link and copy it manually." : copied === "done" ? "Link copied" : ""}</span>
    </div>
  </div>;
}
