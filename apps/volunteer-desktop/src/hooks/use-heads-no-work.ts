import { useApiQuery } from "./use-api";
import type { HeadInfo, ManagementClient } from "../api/client";

/**
 * The attached heads as the daemon reports them (`GET /api/v1/heads`), polled
 * for what each says about sending no work. Empty until the first answer.
 */
export function useHeadsNoWork(intervalMs: number = 15000): HeadInfo[] {
  const { data } = useApiQuery((c: ManagementClient) => c.headsAndMachine(), intervalMs);
  return data?.heads ?? [];
}
