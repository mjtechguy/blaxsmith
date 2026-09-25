import { useId, type ReactNode } from "react";

export function TextField({ label, name, value, onChange, onBlur, autoComplete, placeholder, type = "text", autoFocus, error, hint, trailing, required = true, readOnly }: {
  label: string;
  name: string;
  value: string;
  onChange: (value: string) => void;
  onBlur: () => void;
  autoComplete: string;
  placeholder: string;
  type?: "text" | "password" | "url" | "email";
  readOnly?: boolean;
  autoFocus?: boolean;
  error?: string;
  hint?: string; // Help text under the input.
  trailing?: ReactNode;
  required?: boolean;
}) {
  const id = useId();
  const describedBy = [hint ? `${id}-hint` : "", error ? `${id}-error` : ""].filter(Boolean).join(" ") || undefined;
  const input = <input id={id} type={type} autoFocus={autoFocus} autoComplete={autoComplete} name={name} value={value}
    onBlur={onBlur} onChange={(event) => onChange(event.target.value)} required={required} placeholder={placeholder} readOnly={readOnly}
    aria-invalid={error ? true : undefined} aria-describedby={describedBy} />;
  return <div className="form-field"><label htmlFor={id}>{label}</label>
    {trailing ? <span className="field-with-action">{input}{trailing}</span> : input}
    {hint ? <span id={`${id}-hint`} className="form-hint">{hint}</span> : null}
    {error ? <span id={`${id}-error`} className="form-field-error">{error}</span> : null}
  </div>;
}
