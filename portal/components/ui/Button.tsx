import type { ButtonHTMLAttributes } from "react";

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  busy?: boolean;
};

export function Button({ variant = "secondary", busy = false, className = "", children, ...props }: ButtonProps) {
  const classes = ["ui-button", `ui-button--${variant}`, busy ? "is-busy" : "", className].filter(Boolean).join(" ");
  return (
    <button {...props} className={classes} aria-busy={busy || undefined}>
      {children}
    </button>
  );
}
