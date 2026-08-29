import { useState, useCallback, useRef, useEffect } from "react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Slider } from "@/components/ui/slider";
import type { ScheduleRange } from "@/api/client";

const DAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"] as const;
const HOURS = Array.from({ length: 24 }, (_, i) => i);

type CellKey = `${number}-${number}`; // "day-hour"
function key(day: number, hour: number): CellKey {
  return `${day}-${hour}`;
}

interface ScheduleBuilderProps {
  mode: string;
  idleThresholdMins: number;
  scheduleRanges?: ScheduleRange[];
  onModeChange: (mode: string) => void;
  onIdleThresholdChange: (mins: number) => void;
  onScheduleChange: (ranges: ScheduleRange[]) => void;
}

// Convert ScheduleRange[] to Set<CellKey> for the grid
function rangesToCells(ranges?: ScheduleRange[]): Set<CellKey> {
  const cells = new Set<CellKey>();
  if (!ranges) return cells;
  for (const r of ranges) {
    for (const day of r.days) {
      if (r.start_hour === r.end_hour) {
        // Same start and end = all 24 hours
        for (let h = 0; h < 24; h++) cells.add(key(day, h));
      } else if (r.start_hour < r.end_hour) {
        for (let h = r.start_hour; h < r.end_hour; h++) {
          cells.add(key(day, h));
        }
      } else {
        // Wrapping range (e.g., 22-6)
        for (let h = r.start_hour; h < 24; h++) cells.add(key(day, h));
        for (let h = 0; h < r.end_hour; h++) cells.add(key(day, h));
      }
    }
  }
  return cells;
}

// Convert Set<CellKey> back to ScheduleRange[] (one range per contiguous hour block per day)
function cellsToRanges(cells: Set<CellKey>): ScheduleRange[] {
  const ranges: ScheduleRange[] = [];
  // Group active hours by day
  const byDay = new Map<number, number[]>();
  for (const c of cells) {
    const [d, h] = c.split("-").map(Number);
    if (!byDay.has(d)) byDay.set(d, []);
    byDay.get(d)!.push(h);
  }

  // For each day, find contiguous hour runs
  for (const [day, hours] of byDay) {
    hours.sort((a, b) => a - b);
    let start = hours[0];
    let prev = hours[0];
    for (let i = 1; i <= hours.length; i++) {
      if (i < hours.length && hours[i] === prev + 1) {
        prev = hours[i];
      } else {
        ranges.push({ days: [day], start_hour: start, end_hour: (prev + 1) % 24 });
        if (i < hours.length) {
          start = hours[i];
          prev = hours[i];
        }
      }
    }
  }

  return ranges;
}

// Presets
function nightsPreset(): Set<CellKey> {
  const cells = new Set<CellKey>();
  for (let day = 0; day < 7; day++) {
    for (let h = 22; h < 24; h++) cells.add(key(day, h));
    for (let h = 0; h < 6; h++) cells.add(key(day, h));
  }
  return cells;
}

function weekendsPreset(): Set<CellKey> {
  const cells = new Set<CellKey>();
  for (let h = 0; h < 24; h++) {
    cells.add(key(5, h)); // Sat
    cells.add(key(6, h)); // Sun
  }
  return cells;
}

function nightsAndWeekendsPreset(): Set<CellKey> {
  const cells = nightsPreset();
  for (const c of weekendsPreset()) cells.add(c);
  return cells;
}

function allCells(): Set<CellKey> {
  const cells = new Set<CellKey>();
  for (let d = 0; d < 7; d++)
    for (let h = 0; h < 24; h++) cells.add(key(d, h));
  return cells;
}

export function ScheduleBuilder({
  mode,
  idleThresholdMins,
  scheduleRanges,
  onModeChange,
  onIdleThresholdChange,
  onScheduleChange,
}: ScheduleBuilderProps) {
  const [activeCells, setActiveCells] = useState<Set<CellKey>>(
    () => rangesToCells(scheduleRanges)
  );
  const [dragState, setDragState] = useState<{
    startDay: number;
    startHour: number;
    adding: boolean;
  } | null>(null);
  const [dragEnd, setDragEnd] = useState<{ day: number; hour: number } | null>(null);
  const gridRef = useRef<HTMLDivElement>(null);

  // Sync when scheduleRanges prop changes externally
  useEffect(() => {
    setActiveCells(rangesToCells(scheduleRanges));
  }, [scheduleRanges]);

  const commitCells = useCallback(
    (cells: Set<CellKey>) => {
      setActiveCells(cells);
      onScheduleChange(cellsToRanges(cells));
    },
    [onScheduleChange]
  );

  // Current time indicator
  const now = new Date();
  const currentDay = (now.getDay() + 6) % 7; // Mon=0
  const currentHour = now.getHours();

  // Get cells in drag rectangle
  const getDragRect = useCallback(
    (
      startDay: number,
      startHour: number,
      endDay: number,
      endHour: number
    ): CellKey[] => {
      const minD = Math.min(startDay, endDay);
      const maxD = Math.max(startDay, endDay);
      const minH = Math.min(startHour, endHour);
      const maxH = Math.max(startHour, endHour);
      const keys: CellKey[] = [];
      for (let d = minD; d <= maxD; d++)
        for (let h = minH; h <= maxH; h++) keys.push(key(d, h));
      return keys;
    },
    []
  );

  // Mouse handlers for drag-select
  const handleCellMouseDown = useCallback(
    (day: number, hour: number) => {
      const k = key(day, hour);
      const adding = !activeCells.has(k);
      setDragState({ startDay: day, startHour: hour, adding });
      setDragEnd({ day, hour });
    },
    [activeCells]
  );

  const handleCellMouseEnter = useCallback(
    (day: number, hour: number) => {
      if (dragState) {
        setDragEnd({ day, hour });
      }
    },
    [dragState]
  );

  const handleMouseUp = useCallback(() => {
    if (dragState && dragEnd) {
      const rect = getDragRect(
        dragState.startDay,
        dragState.startHour,
        dragEnd.day,
        dragEnd.hour
      );
      const next = new Set(activeCells);
      for (const k of rect) {
        if (dragState.adding) {
          next.add(k);
        } else {
          next.delete(k);
        }
      }
      commitCells(next);
    }
    setDragState(null);
    setDragEnd(null);
  }, [dragState, dragEnd, activeCells, getDragRect, commitCells]);

  useEffect(() => {
    window.addEventListener("mouseup", handleMouseUp);
    return () => window.removeEventListener("mouseup", handleMouseUp);
  }, [handleMouseUp]);

  // Determine if a cell is in the drag preview
  const isInDragPreview = useCallback(
    (day: number, hour: number): boolean => {
      if (!dragState || !dragEnd) return false;
      const minD = Math.min(dragState.startDay, dragEnd.day);
      const maxD = Math.max(dragState.startDay, dragEnd.day);
      const minH = Math.min(dragState.startHour, dragEnd.hour);
      const maxH = Math.max(dragState.startHour, dragEnd.hour);
      return day >= minD && day <= maxD && hour >= minH && hour <= maxH;
    },
    [dragState, dragEnd]
  );

  const toggleDay = useCallback(
    (day: number) => {
      const next = new Set(activeCells);
      const allActive = HOURS.every((h) => next.has(key(day, h)));
      for (const h of HOURS) {
        const k = key(day, h);
        if (allActive) next.delete(k);
        else next.add(k);
      }
      commitCells(next);
    },
    [activeCells, commitCells]
  );

  const toggleHour = useCallback(
    (hour: number) => {
      const next = new Set(activeCells);
      const allActive = DAYS.every((_, d) => next.has(key(d, hour)));
      for (let d = 0; d < 7; d++) {
        const k = key(d, hour);
        if (allActive) next.delete(k);
        else next.add(k);
      }
      commitCells(next);
    },
    [activeCells, commitCells]
  );

  const modes = [
    { value: "ALWAYS", label: "Always On" },
    { value: "WHEN_IDLE", label: "When Idle" },
    { value: "SCHEDULED", label: "Scheduled" },
  ];

  return (
    <div className="space-y-4">
      {/* Mode tabs */}
      <div className="flex gap-1 rounded-lg bg-muted p-1">
        {modes.map((m) => (
          <button
            key={m.value}
            onClick={() => onModeChange(m.value)}
            className={cn(
              "flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
              mode === m.value
                ? "bg-background text-foreground shadow-sm"
                : "text-muted-foreground hover:text-foreground"
            )}
          >
            {m.label}
          </button>
        ))}
      </div>

      {/* Always On */}
      {mode === "ALWAYS" && (
        <p className="text-sm text-muted-foreground">
          Contributing whenever your computer is on.
        </p>
      )}

      {/* When Idle */}
      {mode === "WHEN_IDLE" && (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            Contributing when no keyboard/mouse activity for{" "}
            <span className="font-medium text-foreground">
              {idleThresholdMins} minutes
            </span>
            .
          </p>
          <Slider
            min={1}
            max={30}
            value={idleThresholdMins}
            onChange={onIdleThresholdChange}
          />
          <div className="flex justify-between text-xs text-muted-foreground">
            <span>1 min</span>
            <span>30 min</span>
          </div>
        </div>
      )}

      {/* Scheduled */}
      {mode === "SCHEDULED" && (
        <div className="space-y-3">
          {/* Presets */}
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => commitCells(nightsPreset())}
            >
              Nights (22-06)
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => commitCells(weekendsPreset())}
            >
              Weekends Only
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => commitCells(nightsAndWeekendsPreset())}
            >
              Nights + Weekends
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => commitCells(allCells())}
            >
              Select All
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => commitCells(new Set())}
            >
              Clear All
            </Button>
          </div>

          {/* Weekly grid */}
          <div
            ref={gridRef}
            className="select-none overflow-x-auto"
            onMouseLeave={() => {
              if (dragState) {
                handleMouseUp();
              }
            }}
          >
            <div className="min-w-[420px]">
              {/* Day headers */}
              <div className="grid grid-cols-[40px_repeat(7,1fr)] gap-px">
                <div /> {/* empty corner */}
                {DAYS.map((day, i) => (
                  <button
                    key={day}
                    onClick={() => toggleDay(i)}
                    className="text-center text-xs font-medium py-1 hover:text-primary transition-colors"
                  >
                    {day}
                  </button>
                ))}
              </div>

              {/* Hour rows */}
              <div className="grid grid-cols-[40px_repeat(7,1fr)] gap-px bg-border rounded-md overflow-hidden">
                {HOURS.map((hour) => (
                  <div key={hour} className="contents">
                    {/* Hour label */}
                    <button
                      onClick={() => toggleHour(hour)}
                      className="bg-background text-xs text-muted-foreground text-right pr-2 py-0.5 hover:text-primary transition-colors flex items-center justify-end"
                    >
                      {hour.toString().padStart(2, "0")}
                    </button>

                    {/* Day cells */}
                    {DAYS.map((_, day) => {
                      const k = key(day, hour);
                      const isActive = activeCells.has(k);
                      const inPreview = isInDragPreview(day, hour);
                      const previewState = inPreview
                        ? dragState?.adding
                        : undefined;
                      const isCurrent =
                        day === currentDay && hour === currentHour;

                      // Visual state: committed state, or preview override
                      const showActive =
                        previewState !== undefined ? previewState : isActive;

                      return (
                        <div
                          key={k}
                          onMouseDown={(e) => {
                            e.preventDefault();
                            handleCellMouseDown(day, hour);
                          }}
                          onMouseEnter={() => handleCellMouseEnter(day, hour)}
                          className={cn(
                            "h-4 cursor-pointer transition-colors",
                            showActive
                              ? "bg-primary/80 hover:bg-primary"
                              : "bg-background hover:bg-muted",
                            isCurrent && "ring-1 ring-inset ring-primary",
                            inPreview && "opacity-80"
                          )}
                        />
                      );
                    })}
                  </div>
                ))}
              </div>
            </div>
          </div>

          <p className="text-xs text-muted-foreground">
            Click cells to toggle. Drag to select a region. Click day/hour
            headers to toggle entire columns/rows.
          </p>
        </div>
      )}
    </div>
  );
}
