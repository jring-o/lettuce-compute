import { render, screen, fireEvent, act } from "@testing-library/react";
import { BrowserControls } from "@/components/projects/browser-controls";

const researchAreas = [
  { id: "ra1", slug: "climate-science", name: "Climate Science", description: null },
  { id: "ra2", slug: "genomics", name: "Genomics", description: null },
  { id: "ra3", slug: "particle-physics", name: "Particle Physics", description: null },
];

const defaultProps = {
  researchAreas,
  search: "",
  researchArea: "",
  sort: "updated_at",
  onSearchChange: jest.fn(),
  onResearchAreaChange: jest.fn(),
  onSortChange: jest.fn(),
};

describe("BrowserControls", () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });

  afterEach(() => {
    jest.useRealTimers();
  });

  it("renders search input with placeholder", () => {
    render(<BrowserControls {...defaultProps} />);
    const input = screen.getByTestId("search-input");
    expect(input).toBeInTheDocument();
    expect(input).toHaveAttribute("placeholder", "Search leafs...");
  });

  it("renders research area dropdown with all areas", () => {
    render(<BrowserControls {...defaultProps} />);
    const select = screen.getByTestId("research-area-filter");
    expect(select).toBeInTheDocument();
    expect(screen.getByText("All Research Areas")).toBeInTheDocument();
    expect(screen.getByText("Climate Science")).toBeInTheDocument();
    expect(screen.getByText("Genomics")).toBeInTheDocument();
    expect(screen.getByText("Particle Physics")).toBeInTheDocument();
  });

  it("renders sort dropdown with correct options", () => {
    render(<BrowserControls {...defaultProps} />);
    const select = screen.getByTestId("sort-select");
    expect(select).toBeInTheDocument();
    expect(screen.getByText("Recently Active")).toBeInTheDocument();
    expect(screen.getByText("Newest")).toBeInTheDocument();
  });

  it("debounces search input before calling onSearchChange", () => {
    const onSearchChange = jest.fn();
    render(<BrowserControls {...defaultProps} onSearchChange={onSearchChange} />);
    const input = screen.getByTestId("search-input");

    fireEvent.change(input, { target: { value: "cl" } });
    expect(onSearchChange).not.toHaveBeenCalled();

    act(() => {
      jest.advanceTimersByTime(300);
    });
    expect(onSearchChange).toHaveBeenCalledWith("cl");
  });

  it("does not trigger search for single character", () => {
    const onSearchChange = jest.fn();
    render(<BrowserControls {...defaultProps} onSearchChange={onSearchChange} />);
    const input = screen.getByTestId("search-input");

    fireEvent.change(input, { target: { value: "c" } });
    act(() => {
      jest.advanceTimersByTime(300);
    });
    expect(onSearchChange).not.toHaveBeenCalled();
  });

  it("shows hint when single character typed", () => {
    render(<BrowserControls {...defaultProps} />);
    const input = screen.getByTestId("search-input");

    fireEvent.change(input, { target: { value: "c" } });
    expect(screen.getByTestId("search-hint")).toHaveTextContent(
      "Type at least 2 characters to search",
    );
  });

  it("calls onSearchChange immediately when clearing", () => {
    const onSearchChange = jest.fn();
    render(<BrowserControls {...defaultProps} search="climate" onSearchChange={onSearchChange} />);

    fireEvent.click(screen.getByTestId("clear-search"));
    expect(onSearchChange).toHaveBeenCalledWith("");
  });

  it("calls onResearchAreaChange when selecting area", () => {
    const onResearchAreaChange = jest.fn();
    render(
      <BrowserControls {...defaultProps} onResearchAreaChange={onResearchAreaChange} />,
    );
    const select = screen.getByTestId("research-area-filter");

    fireEvent.change(select, { target: { value: "genomics" } });
    expect(onResearchAreaChange).toHaveBeenCalledWith("genomics");
  });

  it("calls onSortChange when selecting sort option", () => {
    const onSortChange = jest.fn();
    render(<BrowserControls {...defaultProps} onSortChange={onSortChange} />);
    const select = screen.getByTestId("sort-select");

    fireEvent.change(select, { target: { value: "created_at" } });
    expect(onSortChange).toHaveBeenCalledWith("created_at");
  });

  // --- Additional edge cases ---

  it("cancels previous debounce when typing rapidly", () => {
    const onSearchChange = jest.fn();
    render(<BrowserControls {...defaultProps} onSearchChange={onSearchChange} />);
    const input = screen.getByTestId("search-input");

    fireEvent.change(input, { target: { value: "cl" } });
    act(() => {
      jest.advanceTimersByTime(150);
    });
    fireEvent.change(input, { target: { value: "climate" } });
    act(() => {
      jest.advanceTimersByTime(300);
    });

    // Only the final value should have been emitted
    expect(onSearchChange).toHaveBeenCalledTimes(1);
    expect(onSearchChange).toHaveBeenCalledWith("climate");
  });

  it("does not show clear button when input is empty", () => {
    render(<BrowserControls {...defaultProps} search="" />);
    expect(screen.queryByTestId("clear-search")).not.toBeInTheDocument();
  });

  it("syncs input value when external search prop changes", () => {
    const { rerender } = render(<BrowserControls {...defaultProps} search="" />);
    const input = screen.getByTestId("search-input") as HTMLInputElement;
    expect(input.value).toBe("");

    rerender(<BrowserControls {...defaultProps} search="external" />);
    expect(input.value).toBe("external");
  });

  it("fires onSearchChange for empty string after clearing typed text", () => {
    const onSearchChange = jest.fn();
    render(<BrowserControls {...defaultProps} onSearchChange={onSearchChange} />);
    const input = screen.getByTestId("search-input");

    // Type then erase all characters
    fireEvent.change(input, { target: { value: "cl" } });
    fireEvent.change(input, { target: { value: "" } });
    act(() => {
      jest.advanceTimersByTime(300);
    });

    expect(onSearchChange).toHaveBeenCalledWith("");
  });

  it("cleans up debounce timeout on unmount", () => {
    const onSearchChange = jest.fn();
    const { unmount } = render(
      <BrowserControls {...defaultProps} onSearchChange={onSearchChange} />,
    );
    const input = screen.getByTestId("search-input");

    fireEvent.change(input, { target: { value: "test" } });
    unmount();

    // Advancing timers after unmount should not call the handler
    act(() => {
      jest.advanceTimersByTime(300);
    });
    expect(onSearchChange).not.toHaveBeenCalled();
  });

  it("renders with empty research areas array", () => {
    render(<BrowserControls {...defaultProps} researchAreas={[]} />);
    const select = screen.getByTestId("research-area-filter");
    // Should only have the "All Research Areas" option
    expect(select.querySelectorAll("option")).toHaveLength(1);
  });
});
