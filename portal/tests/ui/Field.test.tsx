import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Field } from "../../components/ui/Field";

describe("Field", () => {
  it("renders label and value", () => {
    render(<Field id="project" label="Project" value="proj_default" onChange={() => {}} />);

    expect(screen.getByLabelText("Project")).toBeInTheDocument();
    expect(screen.getByDisplayValue("proj_default")).toBeInTheDocument();
  });

  it("renders error message", () => {
    const { container } = render(<Field id="repo" label="Repository" value="" onChange={() => {}} error="Repository is required" />);

    expect(screen.getByText("Repository is required")).toBeInTheDocument();
    expect(container.querySelector("#repo")).toHaveAttribute("aria-invalid", "true");
  });
});
