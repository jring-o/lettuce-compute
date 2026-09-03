import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { CreditDisplay, UTC_DAY_NOTE } from "./credit-display";

describe("CreditDisplay", () => {
  const defaultProps = {
    today: 50,
    thisWeek: 200,
    thisMonth: 800,
    total: 5000,
    leafCount: 3,
  };

  it("renders all four stat cards with correct labels", () => {
    render(<CreditDisplay {...defaultProps} />);
    expect(screen.getByText("Today")).toBeInTheDocument();
    expect(screen.getByText("This Week")).toBeInTheDocument();
    expect(screen.getByText("This Month")).toBeInTheDocument();
    expect(screen.getByText("All Time")).toBeInTheDocument();
  });

  it("renders formatted credit values", () => {
    render(<CreditDisplay {...defaultProps} />);
    expect(screen.getByText("50")).toBeInTheDocument();
    expect(screen.getByText("200")).toBeInTheDocument();
    expect(screen.getByText("800")).toBeInTheDocument();
    expect(screen.getByText("5,000")).toBeInTheDocument();
  });

  it("formats large numbers with locale separators", () => {
    render(
      <CreditDisplay
        today={1234}
        thisWeek={56789}
        thisMonth={123456}
        total={1234567}
        leafCount={2}
      />
    );
    expect(screen.getByText("1,234")).toBeInTheDocument();
    expect(screen.getByText("56,789")).toBeInTheDocument();
    expect(screen.getByText("123,456")).toBeInTheDocument();
    expect(screen.getByText("1,234,567")).toBeInTheDocument();
  });

  it("applies highlight styling to All Time card", () => {
    const { container } = render(<CreditDisplay {...defaultProps} />);
    // The All Time card should have highlight classes
    // Find the card containing "All Time" text
    const allTimeLabel = screen.getByText("All Time");
    const card = allTimeLabel.closest("[class*='bg-primary']");
    expect(card).toBeInTheDocument();
  });

  it("renders the All Time value with larger text", () => {
    render(<CreditDisplay {...defaultProps} />);
    const totalValue = screen.getByText("5,000");
    expect(totalValue.className).toContain("text-2xl");
  });

  it("renders non-highlighted values with smaller text", () => {
    render(<CreditDisplay {...defaultProps} />);
    const todayValue = screen.getByText("50");
    expect(todayValue.className).toContain("text-lg");
    expect(todayValue.className).not.toContain("text-2xl");
  });

  it("shows singular 'leaf' when count is 1", () => {
    render(<CreditDisplay {...defaultProps} leafCount={1} />);
    expect(screen.getByText("Across 1 leaf")).toBeInTheDocument();
  });

  it("shows plural 'leafs' when count is not 1", () => {
    render(<CreditDisplay {...defaultProps} leafCount={3} />);
    expect(screen.getByText("Across 3 leafs")).toBeInTheDocument();
  });

  it("shows plural 'leafs' when count is 0", () => {
    render(<CreditDisplay {...defaultProps} leafCount={0} />);
    expect(screen.getByText("Across 0 leafs")).toBeInTheDocument();
  });

  // TB-57: the head records credit by UTC date, the History page groups by the
  // local day, and a volunteer east of Greenwich saw "Today 44" beside a Today
  // group of 48 with nothing saying which calendar either followed.
  it("says the day buckets follow UTC when the daemon cut them by the head's clock", () => {
    render(<CreditDisplay {...defaultProps} dayBoundary="utc" />);
    expect(screen.getByTestId("credit-day-boundary")).toHaveTextContent(UTC_DAY_NOTE);
  });

  it("shows no calendar note when the buckets follow this machine's day", () => {
    render(<CreditDisplay {...defaultProps} dayBoundary="local" />);
    expect(screen.queryByTestId("credit-day-boundary")).not.toBeInTheDocument();
  });
});
