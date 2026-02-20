import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { EmptyState } from "../../components/ui/EmptyState";

describe("EmptyState", () => {
  it("renders title, description, and action", () => {
    render(
      <EmptyState
        title="No data"
        description="Create your first item."
        action={<button type="button">Create</button>}
      />
    );

    expect(screen.getByText("No data")).toBeInTheDocument();
    expect(screen.getByText("Create your first item.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create" })).toBeInTheDocument();
  });
});
