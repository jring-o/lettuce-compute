"use client";

import { useEffect, useRef, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import type { PoolStats } from "@/lib/volunteer/pool-manager";

interface StatusPanelProps {
  stats: PoolStats;
  workerCount: number;
  running: boolean;
}

function formatDuration(totalSeconds: number): string {
  const h = Math.floor(totalSeconds / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  const s = totalSeconds % 60;
  return `${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

export function StatusPanel({ stats, workerCount, running }: StatusPanelProps) {
  const [elapsed, setElapsed] = useState(0);
  const startRef = useRef<number | null>(null);

  useEffect(() => {
    if (running) {
      startRef.current = Date.now();
      const timer = setInterval(() => {
        if (startRef.current) {
          setElapsed(Math.floor((Date.now() - startRef.current) / 1000));
        }
      }, 1000);
      return () => clearInterval(timer);
    } else {
      startRef.current = null;
      // eslint-disable-next-line react-hooks/set-state-in-effect -- intentional reset; component renders null when !running, no cascade risk
      setElapsed(0);
    }
  }, [running]);

  if (!running) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Status</CardTitle>
      </CardHeader>
      <CardContent>
        <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
          <dt className="text-muted-foreground">Active Workers</dt>
          <dd>
            {stats.activeWorkers} / {workerCount}
          </dd>

          <dt className="text-muted-foreground">Work Units Completed</dt>
          <dd>{stats.completedWorkUnits}</dd>

          <dt className="text-muted-foreground">Total Compute Time</dt>
          <dd className="font-mono">{formatDuration(elapsed)}</dd>

          <dt className="text-muted-foreground">Estimated Credit</dt>
          <dd>{stats.completedWorkUnits}</dd>
        </dl>

        {stats.activeWorkers > 0 && (
          <div className="mt-4 space-y-2">
            <p className="text-xs text-muted-foreground">Worker Activity</p>
            {Array.from({ length: workerCount }, (_, i) => (
              <div key={i} className="flex items-center gap-2">
                <span className="w-16 shrink-0 text-xs text-muted-foreground">
                  Worker {i + 1}
                </span>
                <Progress
                  value={i < stats.activeWorkers ? undefined : 0}
                  className={
                    i < stats.activeWorkers
                      ? "animate-pulse"
                      : "opacity-30"
                  }
                />
              </div>
            ))}
          </div>
        )}

        {stats.failedWorkUnits > 0 && (
          <p className="mt-3 text-xs text-destructive">
            {stats.failedWorkUnits} work unit{stats.failedWorkUnits !== 1 ? "s" : ""} failed
          </p>
        )}
      </CardContent>
    </Card>
  );
}
