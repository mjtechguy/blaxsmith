import { useId, type ReactNode } from "react";

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
  const id = useId();
  const input = <input id={id} type={type} autoFocus={autoFocus} autoComplete={autoComplete} name={name} value={value}
    onBlur={onBlur} onChange={(event) => onChange(event.target.value)} required={required} placeholder={placeholder}
    aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-error` : undefined} />;
  return <div className="form-field"><label htmlFor={id}>{label}</label>
    {trailing ? <span className="field-with-action">{input}{trailing}</span> : input}
    {error ? <span id={`${id}-error`} className="form-field-error">{error}</span> : null}
  </div>;
}
