import { Card, CardContent } from "@/components/ui/card";
import { cn, formatCredit } from "@/lib/utils";

interface CreditDisplayProps {
  today: number;
  thisWeek: number;
  thisMonth: number;
  total: number;
  leafCount: number;
}

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
    </div>
  );
}
