import { useEffect, useState } from "react";
import {
  ManagementClient,
  getSystemMetrics,
  type MetricsResponse,
  type SystemMetrics,
} from "../api/client";
import { useApiQuery } from "./use-api";

/**
 * The daemon's `GET /api/v1/metrics`. Only the disk figures are measured
 * there; CPU, memory and temperatures come back as 0. Use `useSystemMetrics`
 * for host CPU and memory.
 */
export function useMetrics(intervalMs: number = 3000): {
  metrics: MetricsResponse | null;
  isLoading: boolean;
  error: Error | null;
} {
  const { data, isLoading, error } = useApiQuery(
    (client: ManagementClient) => client.metrics(),
    intervalMs
  );

  return { metrics: data, isLoading, error };
}

/**
 * Host CPU and memory usage measured by the app's own process (the Rust
 * `system_metrics` command), polled every `intervalMs`. Independent of the
 * daemon, so it works while the daemon is stopped or restarting.
 */
export function useSystemMetrics(intervalMs: number = 3000): {
  system: SystemMetrics | null;
  error: Error | null;
} {
  const [system, setSystem] = useState<SystemMetrics | null>(null);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    let cancelled = false;
    const sample = async () => {
      try {
        const next = await getSystemMetrics();
        if (cancelled) return;
        if (next && typeof next.memory_total_mb === "number") {
          setSystem(next);
          setError(null);
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err : new Error(String(err)));
      }
    };
    sample();
    const id = intervalMs > 0 ? setInterval(sample, intervalMs) : undefined;
    return () => {
      cancelled = true;
      if (id !== undefined) clearInterval(id);
    };
  }, [intervalMs]);

  return { system, error };
}
