import { Card, CardContent } from "@/components/ui/card";
import { cn, formatCredit } from "@/lib/utils";

interface CreditDisplayProps {
  today: number;
  thisWeek: number;
  thisMonth: number;
  total: number;
  leafCount: number;
  /**
   * How the daemon cut the day buckets (`CreditSummary.day_boundary`). Head-derived
   * counters are by UTC date, the History page groups by the local day, and a
   * volunteer east of Greenwich sees the two disagree every night unless the
   * counters say which calendar they follow (TB-57).
   */
  dayBoundary?: "utc" | "local";
}

export const UTC_DAY_NOTE =
  "Days are counted in UTC, the head's clock. The History page groups by your local day.";

function StatCard({
  label,
  value,
  highlight,
}: {
  label: string;
  value: number;
  highlight?: boolean;
}) {
  return (
    <Card className={cn(highlight && "bg-primary/5 border-primary/20")}>
      <CardContent className="p-4 text-center">
        <p className="text-xs text-muted-foreground">{label}</p>
        <p
          className={cn(
            "font-semibold mt-1",
            highlight ? "text-2xl" : "text-lg"
          )}
        >
          {formatCredit(value)}
        </p>
      </CardContent>
    </Card>
  );
}

export function CreditDisplay({
  today,
  thisWeek,
  thisMonth,
  total,
  leafCount,
  dayBoundary,
}: CreditDisplayProps) {
  return (
    <div className="space-y-2">
      <div className="grid grid-cols-4 gap-3">
        <StatCard label="Today" value={today} />
        <StatCard label="This Week" value={thisWeek} />
        <StatCard label="This Month" value={thisMonth} />
        <StatCard label="All Time" value={total} highlight />
      </div>
      <p className="text-xs text-muted-foreground text-center">
        Across {leafCount} leaf{leafCount === 1 ? "" : "s"}
      </p>
      {dayBoundary === "utc" && (
        <p className="text-xs text-muted-foreground text-center" data-testid="credit-day-boundary">
          {UTC_DAY_NOTE}
        </p>
      )}
    </div>
  );
}
