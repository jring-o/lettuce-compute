import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ConfirmDialog } from "./confirm-dialog";

describe("ConfirmDialog", () => {
  it("renders title and description when open", () => {
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={vi.fn()}
        title="Delete Item?"
        description="This action cannot be undone."
        onConfirm={vi.fn()}
      />
    );
    expect(screen.getByText("Delete Item?")).toBeInTheDocument();
    expect(screen.getByText("This action cannot be undone.")).toBeInTheDocument();
  });

  it("does not render content when closed", () => {
    render(
      <ConfirmDialog
        open={false}
        onOpenChange={vi.fn()}
        title="Delete Item?"
        description="This action cannot be undone."
        onConfirm={vi.fn()}
      />
    );
    expect(screen.queryByText("Delete Item?")).not.toBeInTheDocument();
  });

  it("renders default Confirm label when confirmLabel is not provided", () => {
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={vi.fn()}
        title="Are you sure?"
        description="Please confirm."
        onConfirm={vi.fn()}
      />
    );
    expect(screen.getByText("Confirm")).toBeInTheDocument();
  });

  it("renders custom confirmLabel", () => {
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={vi.fn()}
        title="Abort task?"
        description="The task will be killed."
        confirmLabel="Abort"
        onConfirm={vi.fn()}
      />
    );
    expect(screen.getByText("Abort")).toBeInTheDocument();
  });

  it("renders Cancel button", () => {
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={vi.fn()}
        title="Confirm?"
        description="Details here."
        onConfirm={vi.fn()}
      />
    );
    expect(screen.getByText("Cancel")).toBeInTheDocument();
  });

  it("calls onConfirm when confirm button is clicked", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={vi.fn()}
        title="Proceed?"
        description="This will proceed."
        confirmLabel="Yes, Proceed"
        onConfirm={onConfirm}
      />
    );

    await user.click(screen.getByText("Yes, Proceed"));
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("calls onOpenChange(false) when Cancel is clicked", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={onOpenChange}
        title="Cancel me?"
        description="Click cancel."
        onConfirm={vi.fn()}
      />
    );

    await user.click(screen.getByText("Cancel"));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("applies destructive variant styling to confirm button", () => {
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={vi.fn()}
        title="Danger!"
        description="This is dangerous."
        confirmLabel="Delete"
        variant="destructive"
        onConfirm={vi.fn()}
      />
    );
    const deleteButton = screen.getByText("Delete");
    // The destructive variant from buttonVariants should apply destructive classes
    expect(deleteButton.className).toContain("destructive");
  });

  it("applies default variant styling to confirm button when variant is default", () => {
    render(
      <ConfirmDialog
        open={true}
        onOpenChange={vi.fn()}
        title="Normal action"
        description="Nothing scary."
        confirmLabel="OK"
        variant="default"
        onConfirm={vi.fn()}
      />
    );
    const okButton = screen.getByText("OK");
    // Should NOT contain destructive class
    expect(okButton.className).not.toContain("destructive");
  });
});
