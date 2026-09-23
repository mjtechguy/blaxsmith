import type { ReactNode } from "react";

export function TextField({ label, name, value, onChange, onBlur, autoComplete, placeholder, type = "text", autoFocus, error, trailing, required = true }: {
  label: string;
  name: string;
  value: string;
  onChange: (value: string) => void;
  onBlur: () => void;
  autoComplete: string;
  placeholder: string;
  type?: "text" | "password";
  autoFocus?: boolean;
  error?: string;
  trailing?: ReactNode;
  required?: boolean;
}) {
  const input = <input type={type} autoFocus={autoFocus} autoComplete={autoComplete} name={name} value={value}
    onBlur={onBlur} onChange={(event) => onChange(event.target.value)} required={required} placeholder={placeholder}
    aria-invalid={Boolean(error)} aria-describedby={error ? `${name}-error` : undefined} />;
  return <label className="auth-field">{label}
    {trailing ? <span className="auth-password">{input}{trailing}</span> : input}
    {error ? <span id={`${name}-error`} className="auth-field-error">{error}</span> : null}
  </label>;
}
