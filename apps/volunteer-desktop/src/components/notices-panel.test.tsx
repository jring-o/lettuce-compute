import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NoticesPanel } from "./notices-panel";
import type { Notice } from "@/api/client";

const mockUseNotices = vi.fn();
vi.mock("@/hooks/use-notices", () => ({
  useNotices: () => mockUseNotices(),
}));

function notice(overrides: Partial<Notice> & { id: number }): Notice {
  return {
    level: "warn",
    code: "TEST",
    message: `notice ${overrides.id}`,
    count: 1,
    first_at: new Date(Date.now() - 120_000).toISOString(),
    at: new Date(Date.now() - 120_000).toISOString(),
    ...overrides,
  };
}

describe("NoticesPanel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders nothing when there are no notices", () => {
    mockUseNotices.mockReturnValue({ notices: [], supported: true, dismiss: vi.fn() });
    const { container } = render(<NoticesPanel />);
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when the route is unsupported", () => {
    mockUseNotices.mockReturnValue({
      notices: [notice({ id: 1 })],
      supported: false,
      dismiss: vi.fn(),
    });
    const { container } = render(<NoticesPanel />);
    expect(container.firstChild).toBeNull();
  });

  it("shows level, scope, repeat count, relative time and dismisses", async () => {
    const user = userEvent.setup();
    const dismiss = vi.fn();
    mockUseNotices.mockReturnValue({
      supported: true,
      dismiss,
      notices: [
        notice({ id: 9, level: "error", message: "Result rejected", head: "lettuce.science", leaf: "prime", count: 5 }),
        notice({ id: 3, level: "warn", message: "Low disk" }),
      ],
    });
    render(<NoticesPanel />);

    expect(screen.getByText("Needs attention")).toBeInTheDocument();
    expect(screen.getByLabelText("Error")).toBeInTheDocument();
    expect(screen.getByLabelText("Warning")).toBeInTheDocument();
    expect(screen.getByText("Result rejected")).toBeInTheDocument();
    expect(screen.getByText(/lettuce\.science \/ prime/)).toBeInTheDocument();
    expect(screen.getByText(/5 times/)).toBeInTheDocument();
    expect(screen.getAllByText(/2m ago/)).toHaveLength(2);

    const items = screen.getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Result rejected");

    await user.click(screen.getAllByLabelText("Dismiss notice")[1]);
    expect(dismiss).toHaveBeenCalledWith(expect.objectContaining({ id: 3 }));
  });
});
