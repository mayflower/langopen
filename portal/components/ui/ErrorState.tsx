import { Button } from "./Button";

export function ErrorState({ title, message, retry, actionLabel, actionHref }: { title: string; message: string; retry?: () => void; actionLabel?: string; actionHref?: string }) {
  return (
    <section className="ui-error" role="alert">
      <h3>{title}</h3>
      <p>{message}</p>
      <div className="ui-error__actions">
        {retry ? (
          <Button type="button" variant="secondary" onClick={retry}>
            Retry
          </Button>
        ) : null}
        {actionLabel && actionHref ? (
          <a className="ui-button ui-button--primary" href={actionHref}>
            {actionLabel}
          </a>
        ) : null}
      </div>
    </section>
  );
}
