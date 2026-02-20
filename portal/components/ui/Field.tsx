import type { InputHTMLAttributes, ReactNode } from "react";

type FieldProps = InputHTMLAttributes<HTMLInputElement> & {
  id: string;
  label: string;
  hint?: string;
  error?: string;
  prefix?: ReactNode;
  suffix?: ReactNode;
};

export function Field({ id, label, hint, error, prefix, suffix, className = "", ...props }: FieldProps) {
  return (
    <label htmlFor={id} className={["ui-field", error ? "has-error" : "", className].filter(Boolean).join(" ")}>
      <span className="ui-field__label">{label}</span>
      <span className="ui-field__control">
        {prefix ? <span className="ui-field__prefix">{prefix}</span> : null}
        <input id={id} {...props} aria-invalid={error ? true : undefined} />
        {suffix ? <span className="ui-field__suffix">{suffix}</span> : null}
      </span>
      {error ? <span className="ui-field__error">{error}</span> : hint ? <span className="ui-field__hint">{hint}</span> : null}
    </label>
  );
}
