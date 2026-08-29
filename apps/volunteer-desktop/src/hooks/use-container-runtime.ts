import { useState, useEffect, useCallback } from "react";
import {
  getContainerRuntimeStatus,
  type ContainerRuntimeStatus,
} from "../api/client";

export function useContainerRuntime(pollIntervalMs: number = 10000): {
  status: ContainerRuntimeStatus | null;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
} {
  const [status, setStatus] = useState<ContainerRuntimeStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const result = await getContainerRuntimeStatus();
      setStatus(result);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    refresh();
    const interval = setInterval(refresh, pollIntervalMs);
    return () => clearInterval(interval);
  }, [pollIntervalMs, refresh]);

  return { status, loading, error, refresh };
}
