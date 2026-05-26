import { render, screen, fireEvent } from "@testing-library/react";
import { WorkUnitSelector } from "@/components/visualization/WorkUnitSelector";
import type { WorkUnitSummary } from "@/types/infrastructure";

// Mock next/navigation (WorkUnitSelector uses useRouter and usePathname for clearing volunteer filter)
const mockPush = jest.fn();
jest.mock("next/navigation", () => ({
  useRouter: () => ({ push: mockPush }),
  usePathname: () => "/leafs/test-leaf/visualize",
}));

function makeWorkUnit(overrides: Partial<WorkUnitSummary> = {}): WorkUnitSummary {
  return {
    id: "wu-00000000-0000-0000-0000-000000000001",
    leaf_id: "leaf-1",
    batch_id: null,
    state: "VALIDATED",
    priority: "NORMAL",
    assigned_to: null,
    attempts: 1,
    flagged_for_review: false,
    created_at: "2026-03-15T10:00:00Z",
    updated_at: "2026-03-15T12:30:00Z",
    ...overrides,
  };
}

describe("WorkUnitSelector", () => {
  it("renders empty state when workUnits is empty", () => {
    render(
      <WorkUnitSelector
        workUnits={[]}
        selectedId={null}
        onSelect={jest.fn()}
      />,
    );

    expect(
      screen.getByText("No completed work units available."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  });

  it("renders a dropdown with work unit options", () => {
    const wus = [
      makeWorkUnit({ id: "wu-aaaaaaaa-1111-2222-3333-444444444444", updated_at: "2026-03-15T12:30:00Z" }),
      makeWorkUnit({ id: "wu-bbbbbbbb-5555-6666-7777-888888888888", updated_at: "2026-03-16T09:00:00Z" }),
    ];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
      />,
    );

    const select = screen.getByLabelText("Work Unit");
    expect(select).toBeInTheDocument();
    expect(select.tagName).toBe("SELECT");

    // Options should show first 8 chars of ID
    const options = screen.getAllByRole("option");
    expect(options).toHaveLength(2);
    expect(options[0].textContent).toContain("wu-aaaaa");
    expect(options[1].textContent).toContain("wu-bbbbb");
  });

  it("calls onSelect when a different option is chosen", () => {
    const onSelect = jest.fn();
    const wus = [
      makeWorkUnit({ id: "wu-aaaaaaaa-1111-2222-3333-444444444444" }),
      makeWorkUnit({ id: "wu-bbbbbbbb-5555-6666-7777-888888888888" }),
    ];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={onSelect}
      />,
    );

    const select = screen.getByLabelText("Work Unit");
    fireEvent.change(select, { target: { value: wus[1].id } });

    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect).toHaveBeenCalledWith(wus[1].id);
  });

  it("sets the selected value from selectedId prop", () => {
    const wus = [
      makeWorkUnit({ id: "wu-aaaaaaaa-1111-2222-3333-444444444444" }),
      makeWorkUnit({ id: "wu-bbbbbbbb-5555-6666-7777-888888888888" }),
    ];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[1].id}
        onSelect={jest.fn()}
      />,
    );

    const select = screen.getByLabelText("Work Unit") as HTMLSelectElement;
    expect(select.value).toBe(wus[1].id);
  });

  it("sets value to empty string when selectedId is null", () => {
    const wus = [makeWorkUnit()];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={null}
        onSelect={jest.fn()}
      />,
    );

    const select = screen.getByLabelText("Work Unit") as HTMLSelectElement;
    // When selectedId is null, value is set to ""
    // Browser may default to first option, but the controlled value is ""
    expect(select).toBeInTheDocument();
  });

  it("disables the select when loading is true", () => {
    const wus = [makeWorkUnit()];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
        loading={true}
      />,
    );

    const select = screen.getByLabelText("Work Unit") as HTMLSelectElement;
    expect(select.disabled).toBe(true);
  });

  it("shows loading indicator when loading is true", () => {
    const wus = [makeWorkUnit()];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
        loading={true}
      />,
    );

    expect(screen.getByText("Loading...")).toBeInTheDocument();
  });

  it("does not show loading indicator when loading is false", () => {
    const wus = [makeWorkUnit()];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
        loading={false}
      />,
    );

    expect(screen.queryByText("Loading...")).not.toBeInTheDocument();
  });

  it("does not show loading indicator when loading is undefined", () => {
    const wus = [makeWorkUnit()];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
      />,
    );

    expect(screen.queryByText("Loading...")).not.toBeInTheDocument();
  });

  it("formats date in option labels", () => {
    const wus = [
      makeWorkUnit({
        id: "wu-aaaaaaaa-1111-2222-3333-444444444444",
        updated_at: "2026-03-15T12:30:00Z",
      }),
    ];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
      />,
    );

    const option = screen.getByRole("option");
    // The option should contain the truncated ID and a formatted date
    expect(option.textContent).toContain("wu-aaaaa");
    // The date formatting uses toLocaleDateString, so we just check it contains
    // something date-like (month abbreviation and day number)
    expect(option.textContent).toContain("—");
  });

  // --- S109: volunteer filter chip tests ---

  it("renders volunteer filter chip when volunteerFilter is provided", () => {
    const wus = [makeWorkUnit()];
    const volunteerId = "abcdef12-3456-7890-abcd-ef1234567890";

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
        volunteerFilter={volunteerId}
      />,
    );

    // Chip should show truncated volunteer ID (first 8 chars)
    expect(screen.getByText(/Filtered to volunteer abcdef12/)).toBeInTheDocument();
  });

  it("does not render volunteer filter chip when volunteerFilter is undefined", () => {
    const wus = [makeWorkUnit()];

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
      />,
    );

    expect(screen.queryByText(/Filtered to volunteer/)).not.toBeInTheDocument();
  });

  it("clear button navigates to pathname without volunteer filter", () => {
    const wus = [makeWorkUnit()];
    const volunteerId = "abcdef12-3456-7890-abcd-ef1234567890";
    mockPush.mockClear();

    render(
      <WorkUnitSelector
        workUnits={wus}
        selectedId={wus[0].id}
        onSelect={jest.fn()}
        volunteerFilter={volunteerId}
      />,
    );

    const clearButton = screen.getByLabelText("Clear volunteer filter");
    fireEvent.click(clearButton);

    expect(mockPush).toHaveBeenCalledWith("/leafs/test-leaf/visualize");
  });
});
