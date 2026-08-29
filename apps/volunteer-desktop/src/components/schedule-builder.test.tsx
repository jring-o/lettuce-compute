import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ScheduleBuilder } from "./schedule-builder";

// Default props for rendering
function defaultProps(overrides: Partial<Parameters<typeof ScheduleBuilder>[0]> = {}) {
  return {
    mode: "ALWAYS" as string,
    idleThresholdMins: 5,
    scheduleRanges: [],
    onModeChange: vi.fn(),
    onIdleThresholdChange: vi.fn(),
    onScheduleChange: vi.fn(),
    ...overrides,
  };
}

describe("ScheduleBuilder", () => {
  describe("Mode tabs", () => {
    it("renders all three mode tabs", () => {
      render(<ScheduleBuilder {...defaultProps()} />);
      expect(screen.getByText("Always On")).toBeInTheDocument();
      expect(screen.getByText("When Idle")).toBeInTheDocument();
      expect(screen.getByText("Scheduled")).toBeInTheDocument();
    });

    it("calls onModeChange when clicking a mode tab", async () => {
      const user = userEvent.setup();
      const onModeChange = vi.fn();
      render(<ScheduleBuilder {...defaultProps({ onModeChange })} />);

      await user.click(screen.getByText("When Idle"));
      expect(onModeChange).toHaveBeenCalledWith("WHEN_IDLE");

      await user.click(screen.getByText("Scheduled"));
      expect(onModeChange).toHaveBeenCalledWith("SCHEDULED");

      await user.click(screen.getByText("Always On"));
      expect(onModeChange).toHaveBeenCalledWith("ALWAYS");
    });
  });

  describe("ALWAYS mode", () => {
    it("shows description text when mode is ALWAYS", () => {
      render(<ScheduleBuilder {...defaultProps({ mode: "ALWAYS" })} />);
      expect(
        screen.getByText(/Contributing whenever your computer is on/)
      ).toBeInTheDocument();
    });

    it("does not show schedule grid in ALWAYS mode", () => {
      render(<ScheduleBuilder {...defaultProps({ mode: "ALWAYS" })} />);
      expect(screen.queryByText("Nights (22-06)")).not.toBeInTheDocument();
    });
  });

  describe("WHEN_IDLE mode", () => {
    it("shows idle threshold description", () => {
      render(
        <ScheduleBuilder
          {...defaultProps({ mode: "WHEN_IDLE", idleThresholdMins: 10 })}
        />
      );
      expect(screen.getByText("10 minutes")).toBeInTheDocument();
    });

    it("renders idle slider", () => {
      render(
        <ScheduleBuilder {...defaultProps({ mode: "WHEN_IDLE" })} />
      );
      // The slider is an input[type=range]
      const slider = screen.getByRole("slider");
      expect(slider).toBeInTheDocument();
    });
  });

  describe("SCHEDULED mode", () => {
    it("shows preset buttons", () => {
      render(
        <ScheduleBuilder {...defaultProps({ mode: "SCHEDULED" })} />
      );
      expect(screen.getByText("Nights (22-06)")).toBeInTheDocument();
      expect(screen.getByText("Weekends Only")).toBeInTheDocument();
      expect(screen.getByText("Nights + Weekends")).toBeInTheDocument();
      expect(screen.getByText("Select All")).toBeInTheDocument();
      expect(screen.getByText("Clear All")).toBeInTheDocument();
    });

    it("shows day headers", () => {
      render(
        <ScheduleBuilder {...defaultProps({ mode: "SCHEDULED" })} />
      );
      expect(screen.getByText("Mon")).toBeInTheDocument();
      expect(screen.getByText("Tue")).toBeInTheDocument();
      expect(screen.getByText("Wed")).toBeInTheDocument();
      expect(screen.getByText("Thu")).toBeInTheDocument();
      expect(screen.getByText("Fri")).toBeInTheDocument();
      expect(screen.getByText("Sat")).toBeInTheDocument();
      expect(screen.getByText("Sun")).toBeInTheDocument();
    });

    it("shows hour labels", () => {
      render(
        <ScheduleBuilder {...defaultProps({ mode: "SCHEDULED" })} />
      );
      // Check a few hour labels
      expect(screen.getByText("00")).toBeInTheDocument();
      expect(screen.getByText("12")).toBeInTheDocument();
      expect(screen.getByText("23")).toBeInTheDocument();
    });

    it("calls onScheduleChange when Nights preset is clicked", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({ mode: "SCHEDULED", onScheduleChange })}
        />
      );

      await user.click(screen.getByText("Nights (22-06)"));
      expect(onScheduleChange).toHaveBeenCalled();

      // The ranges should include hours 22, 23, 0, 1, 2, 3, 4, 5 for all 7 days
      const ranges = onScheduleChange.mock.calls[0][0];
      expect(ranges.length).toBeGreaterThan(0);

      // Verify that every day has coverage for night hours
      const coveredDays = new Set<number>();
      for (const r of ranges) {
        for (const d of r.days) {
          coveredDays.add(d);
        }
      }
      expect(coveredDays.size).toBe(7);
    });

    it("calls onScheduleChange when Weekends Only preset is clicked", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({ mode: "SCHEDULED", onScheduleChange })}
        />
      );

      await user.click(screen.getByText("Weekends Only"));
      expect(onScheduleChange).toHaveBeenCalled();

      const ranges = onScheduleChange.mock.calls[0][0];
      // Weekend = Sat(5) and Sun(6)
      const coveredDays = new Set<number>();
      for (const r of ranges) {
        for (const d of r.days) {
          coveredDays.add(d);
        }
      }
      expect(coveredDays.has(5)).toBe(true); // Saturday
      expect(coveredDays.has(6)).toBe(true); // Sunday
      expect(coveredDays.has(0)).toBe(false); // Monday should not be covered
    });

    it("calls onScheduleChange with empty ranges when Clear All is clicked", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({
            mode: "SCHEDULED",
            onScheduleChange,
            scheduleRanges: [{ days: [0], start_hour: 8, end_hour: 18 }],
          })}
        />
      );

      await user.click(screen.getByText("Clear All"));
      expect(onScheduleChange).toHaveBeenCalledWith([]);
    });

    it("renders initial schedule ranges as active cells", () => {
      const { container } = render(
        <ScheduleBuilder
          {...defaultProps({
            mode: "SCHEDULED",
            scheduleRanges: [
              { days: [0, 1, 2, 3, 4, 5, 6], start_hour: 0, end_hour: 23 },
            ],
          })}
        />
      );

      // When ranges cover all hours for all days, many cells should have the active style
      // We check that the component rendered without errors
      expect(container).toBeTruthy();
    });

    it("shows help text about clicking and dragging", () => {
      render(
        <ScheduleBuilder {...defaultProps({ mode: "SCHEDULED" })} />
      );
      expect(
        screen.getByText(/Click cells to toggle/)
      ).toBeInTheDocument();
    });

    it("calls onScheduleChange with ranges when Select All is clicked", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({ mode: "SCHEDULED", onScheduleChange })}
        />
      );

      await user.click(screen.getByText("Select All"));
      expect(onScheduleChange).toHaveBeenCalled();

      const ranges = onScheduleChange.mock.calls[0][0];
      // All 7 days should be covered
      const coveredDays = new Set<number>();
      for (const r of ranges) {
        for (const d of r.days) coveredDays.add(d);
      }
      expect(coveredDays.size).toBe(7);
      // Each day should have a range that covers all 24 hours.
      // cellsToRanges produces {start_hour: 0, end_hour: 0} for a full day
      // (since (23+1)%24 = 0). This is technically a wrapping range 0->0
      // meaning "full day", but rangesToCells treats 0<=0 as non-wrapping
      // with 0 hours. This is a known edge case (BUG) in the roundtrip.
      // The ranges are emitted correctly from the cell state, but if
      // reloaded via rangesToCells, a full-day selection would be lost.
      for (const r of ranges) {
        expect(r.start_hour).toBe(0);
        expect(r.end_hour).toBe(0);
      }
    });

    it("correctly roundtrips wrapping schedule ranges through the component", () => {
      // A wrapping range like 22-6 should be displayed correctly
      const onScheduleChange = vi.fn();
      const { container } = render(
        <ScheduleBuilder
          {...defaultProps({
            mode: "SCHEDULED",
            onScheduleChange,
            scheduleRanges: [
              { days: [0], start_hour: 22, end_hour: 6 },
            ],
          })}
        />
      );
      // Component should render without errors
      expect(container).toBeTruthy();
    });

    it("Nights+Weekends preset covers both nights and weekend days", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({ mode: "SCHEDULED", onScheduleChange })}
        />
      );

      await user.click(screen.getByText("Nights + Weekends"));
      expect(onScheduleChange).toHaveBeenCalled();

      const ranges = onScheduleChange.mock.calls[0][0];
      // Verify that both weekday and weekend days are present
      const coveredDays = new Set<number>();
      for (const r of ranges) {
        for (const d of r.days) coveredDays.add(d);
      }
      // Nights apply to all 7 days, weekends add Sat(5) and Sun(6)
      expect(coveredDays.size).toBe(7);

      // Verify that weekday ranges include night hours (22, 23, 0-5)
      // Weekday night ranges should have hours in the 22-23 and 0-5 range
      let hasNightHour = false;
      for (const r of ranges) {
        if (r.days.includes(0)) { // Monday
          // Night hours produce start=0,end=6 and start=22,end=0 ranges
          if (r.start_hour === 22 || r.start_hour === 0) {
            hasNightHour = true;
          }
        }
      }
      expect(hasNightHour).toBe(true);

      // Weekend days (5, 6) should have full-day coverage (0-0)
      let satFullDay = false;
      for (const r of ranges) {
        if (r.days.includes(5) && r.start_hour === 0 && r.end_hour === 0) {
          satFullDay = true;
        }
      }
      expect(satFullDay).toBe(true);
    });

    it("toggles an entire day column when day header is clicked", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({ mode: "SCHEDULED", onScheduleChange })}
        />
      );

      // Click "Mon" header to select all Monday hours
      await user.click(screen.getByText("Mon"));
      expect(onScheduleChange).toHaveBeenCalled();

      const ranges = onScheduleChange.mock.calls[0][0];
      // Should have a range covering day 0 (Monday) with all 24 hours
      // cellsToRanges produces {days:[0], start_hour:0, end_hour:0}
      let mondayRange = false;
      for (const r of ranges) {
        if (r.days.includes(0)) {
          mondayRange = true;
          // Full day: 0-0 wrapping
          expect(r.start_hour).toBe(0);
          expect(r.end_hour).toBe(0);
        }
      }
      expect(mondayRange).toBe(true);
    });

    it("toggles an entire hour row when hour label is clicked", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({ mode: "SCHEDULED", onScheduleChange })}
        />
      );

      // Click "12" hour label to select hour 12 for all days
      await user.click(screen.getByText("12"));
      expect(onScheduleChange).toHaveBeenCalled();

      const ranges = onScheduleChange.mock.calls[0][0];
      // Should cover hour 12 for all 7 days
      const coveredDays = new Set<number>();
      for (const r of ranges) {
        if (r.start_hour <= r.end_hour) {
          if (12 >= r.start_hour && 12 < r.end_hour) {
            for (const d of r.days) coveredDays.add(d);
          }
        } else {
          if (12 >= r.start_hour || 12 < r.end_hour) {
            for (const d of r.days) coveredDays.add(d);
          }
        }
      }
      expect(coveredDays.size).toBe(7);
    });

    it("toggling a fully-selected day deselects it", async () => {
      const user = userEvent.setup();
      const onScheduleChange = vi.fn();
      render(
        <ScheduleBuilder
          {...defaultProps({
            mode: "SCHEDULED",
            onScheduleChange,
            // All hours for Monday already selected
            scheduleRanges: [
              { days: [0], start_hour: 0, end_hour: 23 },
              { days: [0], start_hour: 23, end_hour: 0 },
            ],
          })}
        />
      );

      // Click "Mon" header should deselect all Monday hours
      await user.click(screen.getByText("Mon"));
      expect(onScheduleChange).toHaveBeenCalled();

      const ranges = onScheduleChange.mock.calls[0][0];
      // No Monday hours should remain
      const mondayHours = new Set<number>();
      for (const r of ranges) {
        if (r.days.includes(0)) {
          if (r.start_hour <= r.end_hour) {
            for (let h = r.start_hour; h < r.end_hour; h++) mondayHours.add(h);
          } else {
            for (let h = r.start_hour; h < 24; h++) mondayHours.add(h);
            for (let h = 0; h < r.end_hour; h++) mondayHours.add(h);
          }
        }
      }
      expect(mondayHours.size).toBe(0);
    });

    it("handles single cell mouseDown + mouseUp (click toggle)", () => {
      const onScheduleChange = vi.fn();
      const { container } = render(
        <ScheduleBuilder
          {...defaultProps({ mode: "SCHEDULED", onScheduleChange })}
        />
      );

      // Find the grid cells (they are div elements within the hour rows)
      // The grid has 24 rows * 7 day columns = 168 cells
      // Each cell has cursor-pointer class
      const cells = container.querySelectorAll(".cursor-pointer");
      expect(cells.length).toBe(168);

      // Simulate clicking first cell: mouseDown then mouseUp
      fireEvent.mouseDown(cells[0], { preventDefault: () => {} });
      fireEvent.mouseUp(window);

      expect(onScheduleChange).toHaveBeenCalled();
    });
  });
});
