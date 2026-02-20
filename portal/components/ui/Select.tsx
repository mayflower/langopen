import type { ReactNode, SelectHTMLAttributes } from "react";

type SelectProps = SelectHTMLAttributes<HTMLSelectElement> & {
  id: string;
  label: string;
  hint?: string;
  error?: string;
  children: ReactNode;
};

export function Select({ id, label, hint, error, className = "", children, ...props }: SelectProps) {
  return (
    <label htmlFor={id} className={["ui-field", error ? "has-error" : "", className].filter(Boolean).join(" ")}>
      <span className="ui-field__label">{label}</span>
      <span className="ui-field__control">
        <select id={id} {...props} aria-invalid={error ? true : undefined}>
          {children}
        </select>
      </span>
      {error ? <span className="ui-field__error">{error}</span> : hint ? <span className="ui-field__hint">{hint}</span> : null}
    </label>
  );
}
