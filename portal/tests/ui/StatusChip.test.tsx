import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { StatusChip } from "../../components/ui/StatusChip";

describe("StatusChip", () => {
  it("maps failed status to error class", () => {
    render(<StatusChip status="failed" />);

    const chip = screen.getByText("failed");
    expect(chip).toHaveClass("ui-status--error");
  });

  it("maps queued status to queued class", () => {
    render(<StatusChip status="queued" />);

    const chip = screen.getByText("queued");
    expect(chip).toHaveClass("ui-status--queued");
  });
});
