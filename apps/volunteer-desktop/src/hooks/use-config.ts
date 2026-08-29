import { useCallback, useState } from "react";
import { useApiQuery } from "./use-api";
import type { ConfigResponse } from "../api/client";
import { useClient } from "./use-api";

export function useConfig() {
  const { client } = useClient();
  const {
    data: config,
    isLoading,
    error,
    refetch,
  } = useApiQuery<ConfigResponse>((c) => c.config());
  const [saving, setSaving] = useState(false);
  const [toast, setToast] = useState<string | null>(null);

  const updateConfig = useCallback(
    async (partial: Partial<ConfigResponse>) => {
      if (!client) return;
      setSaving(true);
      try {
        await client.updateConfig(partial);
        refetch();
        setToast("Saved");
        setTimeout(() => setToast(null), 2000);
      } catch (err) {
        setToast(
          err instanceof Error ? `Error: ${err.message}` : "Save failed"
        );
        setTimeout(() => setToast(null), 3000);
      } finally {
        setSaving(false);
      }
    },
    [client, refetch]
  );

  return { config, isLoading, error, updateConfig, saving, toast, refetch };
}
