import { ManagementClient, type CreditSummary } from "../api/client";
import { useApiQuery } from "./use-api";

export function useCredit(intervalMs: number = 10000): {
  credit: CreditSummary | null;
  isLoading: boolean;
  error: Error | null;
} {
  const { data, isLoading, error } = useApiQuery(
    (client: ManagementClient) => client.credit(),
    intervalMs
  );

  return { credit: data, isLoading, error };
}
