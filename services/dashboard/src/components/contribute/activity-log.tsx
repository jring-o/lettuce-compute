"use client";

import { useEffect, useRef } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export interface LogEntry {
  id: number;
  timestamp: Date;
  message: string;
  level: "info" | "error" | "success";
}

interface ActivityLogProps {
  entries: LogEntry[];
}

export function ActivityLog({ entries }: ActivityLogProps) {
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = 0;
    }
  }, [entries.length]);

  if (entries.length === 0) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Activity Log</CardTitle>
      </CardHeader>
      <CardContent>
        <div
          ref={scrollRef}
          className="max-h-48 overflow-y-auto font-mono text-xs"
        >
          {entries.map((entry) => (
            <div
              key={entry.id}
              className={`border-b border-border/50 py-1 last:border-0 ${levelColor(entry.level)}`}
            >
              <span className="text-muted-foreground">
                {formatTime(entry.timestamp)}
              </span>{" "}
              {entry.message}
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

function levelColor(level: LogEntry["level"]): string {
  switch (level) {
    case "error":
      return "text-destructive";
    case "success":
      return "text-green-600 dark:text-green-400";
    default:
      return "";
  }
}

function formatTime(date: Date): string {
  return date.toLocaleTimeString("en-US", { hour12: false });
}
