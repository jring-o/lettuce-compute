import { ManagementClient, type MetricsResponse } from "../api/client";
import { useApiQuery } from "./use-api";

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
