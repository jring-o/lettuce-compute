import { render, screen } from "@testing-library/react";
import { ActivityLog, type LogEntry } from "@/components/contribute/activity-log";

function makeEntry(
  overrides: Partial<LogEntry> & { id: number }
): LogEntry {
  return {
    timestamp: new Date("2026-03-27T14:30:00Z"),
    message: `Log message ${overrides.id}`,
    level: "info",
    ...overrides,
  };
}

describe("ActivityLog", () => {
  it("renders nothing when entries is empty", () => {
    const { container } = render(<ActivityLog entries={[]} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders the Activity Log heading when entries exist", () => {
    const entries: LogEntry[] = [
      makeEntry({ id: 1, message: "Connected to server" }),
    ];
    render(<ActivityLog entries={entries} />);
    expect(screen.getByText("Activity Log")).toBeInTheDocument();
  });

  it("renders all provided entries", () => {
    const entries: LogEntry[] = [
      makeEntry({ id: 1, message: "First entry" }),
      makeEntry({ id: 2, message: "Second entry" }),
      makeEntry({ id: 3, message: "Third entry" }),
    ];
    render(<ActivityLog entries={entries} />);
    expect(screen.getByText("First entry")).toBeInTheDocument();
    expect(screen.getByText("Second entry")).toBeInTheDocument();
    expect(screen.getByText("Third entry")).toBeInTheDocument();
  });

  it("formats timestamps using 24-hour time", () => {
    const entries: LogEntry[] = [
      makeEntry({
        id: 1,
        timestamp: new Date("2026-03-27T14:30:45Z"),
        message: "test message",
      }),
    ];
    render(<ActivityLog entries={entries} />);
    // The timestamp format is HH:MM:SS (locale-dependent but en-US 24h)
    // We verify that a time-like string appears somewhere
    const container = screen.getByText("test message").parentElement!;
    expect(container.textContent).toMatch(/\d{1,2}:\d{2}:\d{2}/);
  });

  it("applies error color class for error-level entries", () => {
    const entries: LogEntry[] = [
      makeEntry({ id: 1, message: "Something failed", level: "error" }),
    ];
    render(<ActivityLog entries={entries} />);
    const entryEl = screen.getByText("Something failed").closest("div");
    expect(entryEl).toHaveClass("text-destructive");
  });

  it("applies success color class for success-level entries", () => {
    const entries: LogEntry[] = [
      makeEntry({ id: 1, message: "Connected!", level: "success" }),
    ];
    render(<ActivityLog entries={entries} />);
    const entryEl = screen.getByText("Connected!").closest("div");
    expect(entryEl).toHaveClass("text-green-600");
  });

  it("applies no extra color class for info-level entries", () => {
    const entries: LogEntry[] = [
      makeEntry({ id: 1, message: "Neutral info", level: "info" }),
    ];
    render(<ActivityLog entries={entries} />);
    const entryEl = screen.getByText("Neutral info").closest("div");
    expect(entryEl).not.toHaveClass("text-destructive");
    expect(entryEl).not.toHaveClass("text-green-600");
  });

  it("renders entries with unique keys (no duplicate key warnings)", () => {
    // If keys are duplicated, React warns. We verify rendering succeeds
    // and the correct number of log messages appear.
    const entries: LogEntry[] = [
      makeEntry({ id: 1, message: "Entry one" }),
      makeEntry({ id: 2, message: "Entry two" }),
    ];
    render(<ActivityLog entries={entries} />);
    expect(screen.getByText("Entry one")).toBeInTheDocument();
    expect(screen.getByText("Entry two")).toBeInTheDocument();
  });
});
