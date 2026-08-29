import { ManagementClient, type StatusResponse } from "../api/client";
import { useApiQuery } from "./use-api";

export function useDaemonStatus(intervalMs: number = 3000): {
  status: StatusResponse | null;
  isLoading: boolean;
  error: Error | null;
  refetch: () => void;
} {
  const { data, isLoading, error, refetch } = useApiQuery(
    (client: ManagementClient) => client.status(),
    intervalMs
  );

  return { status: data, isLoading, error, refetch };
}
