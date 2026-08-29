import { cn } from "@/lib/utils";

interface ResourceGaugeProps {
  label: string;
  value: number;
  displayValue: string;
  temperature?: number;
  colorThresholds?: { green: number; yellow: number };
  size?: "sm" | "md" | "lg";
}

const sizes = {
  sm: { width: 100, stroke: 8, fontSize: 12, labelSize: 10 },
  md: { width: 130, stroke: 10, fontSize: 14, labelSize: 12 },
  lg: { width: 160, stroke: 12, fontSize: 16, labelSize: 14 },
};

function gaugeColor(
  value: number,
  thresholds: { green: number; yellow: number }
): string {
  if (value >= thresholds.yellow) return "hsl(0, 84%, 60%)";
  if (value >= thresholds.green) return "hsl(45, 93%, 47%)";
  return "hsl(142, 76%, 36%)";
}

export function ResourceGauge({
  label,
  value,
  displayValue,
  temperature,
  colorThresholds = { green: 70, yellow: 90 },
  size = "md",
}: ResourceGaugeProps) {
  const s = sizes[size];
  const center = s.width / 2;
  const radius = (s.width - s.stroke) / 2 - 4;

  // Arc from 135° to 405° (270° sweep)
  const startAngle = 135;
  const sweepAngle = 270;
  const clamped = Math.min(100, Math.max(0, value));
  const endAngle = startAngle + (sweepAngle * clamped) / 100;

  const toXY = (angleDeg: number) => {
    const rad = (angleDeg * Math.PI) / 180;
    return {
      x: center + radius * Math.cos(rad),
      y: center + radius * Math.sin(rad),
    };
  };

  const arcPath = (start: number, end: number) => {
    const s1 = toXY(start);
    const s2 = toXY(end);
    const sweep = end - start;
    const largeArc = sweep > 180 ? 1 : 0;
    return `M ${s1.x} ${s1.y} A ${radius} ${radius} 0 ${largeArc} 1 ${s2.x} ${s2.y}`;
  };

  const color = gaugeColor(clamped, colorThresholds);

  return (
    <div className="flex flex-col items-center gap-1">
      <svg width={s.width} height={s.width} viewBox={`0 0 ${s.width} ${s.width}`}>
        {/* Background track */}
        <path
          d={arcPath(startAngle, startAngle + sweepAngle)}
          fill="none"
          stroke="hsl(var(--muted))"
          strokeWidth={s.stroke}
          strokeLinecap="round"
        />
        {/* Value arc */}
        {clamped > 0 && (
          <path
            d={arcPath(startAngle, endAngle)}
            fill="none"
            stroke={color}
            strokeWidth={s.stroke}
            strokeLinecap="round"
            style={{
              transition: "d 300ms ease, stroke 300ms ease",
            }}
          />
        )}
        {/* Center text */}
        <text
          x={center}
          y={center - 4}
          textAnchor="middle"
          dominantBaseline="middle"
          fill="currentColor"
          fontSize={s.fontSize}
          fontWeight="600"
        >
          {displayValue}
        </text>
        <text
          x={center}
          y={center + s.fontSize}
          textAnchor="middle"
          dominantBaseline="middle"
          fill="hsl(var(--muted-foreground))"
          fontSize={s.labelSize}
        >
          {label}
        </text>
      </svg>
      {temperature != null && temperature > 0 && (
        <span
          className={cn(
            "text-xs text-muted-foreground",
            temperature >= 85 && "text-red-500",
            temperature >= 70 && temperature < 85 && "text-yellow-500"
          )}
        >
          {label} {temperature}°C
        </span>
      )}
    </div>
  );
}
