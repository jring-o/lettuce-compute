import { useCallback, useRef, useState, useEffect } from "react";
import { useClient } from "./use-api";
import type { HeadInfo, LeafPreferences, ConfigResponse } from "../api/client";

export function useHeads(): {
  heads: HeadInfo[];
  isLoading: boolean;
  error: Error | null;
  refetch: () => void;
  setHeads: React.Dispatch<React.SetStateAction<HeadInfo[]>>;
} {
  const { client } = useClient();
  const [heads, setHeads] = useState<HeadInfo[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const fetchedRef = useRef(false);

  const fetchHeads = useCallback(async () => {
    if (!client) return;
    try {
      const result = await client.heads();
      setHeads(result);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err : new Error("Failed to fetch heads"));
    } finally {
      setIsLoading(false);
    }
  }, [client]);

  // Fetch once on mount — local state is authoritative after that
  useEffect(() => {
    if (client && !fetchedRef.current) {
      fetchedRef.current = true;
      fetchHeads();
    }
  }, [client, fetchHeads]);

  return { heads, isLoading, error, refetch: fetchHeads, setHeads };
}

// Helper: read current config, apply a server-level update, write it back.
async function writeServerConfig(
  client: { config: () => Promise<ConfigResponse>; updateConfig: (partial: Record<string, unknown>) => Promise<unknown> },
  serverName: string,
  apply: (server: ConfigResponse["servers"][0]) => ConfigResponse["servers"][0]
) {
  const config = await client.config();
  const servers = config.servers.map((s) =>
    s.name === serverName ? apply(s) : s
  );
  await client.updateConfig({ servers });
}

// Immediate write — for discrete actions (checkbox toggle).
export function useWriteLeafPreferences(): {
  write: (serverName: string, prefs: LeafPreferences) => Promise<void>;
} {
  const { client } = useClient();

  const write = useCallback(
    async (serverName: string, prefs: LeafPreferences) => {
      if (!client) return;
      await writeServerConfig(client, serverName, (s) => ({
        ...s,
        leaf_preferences: prefs,
      }));
    },
    [client]
  );

  return { write };
}

// Debounced write — for continuous inputs (sliders).
export function useDebouncedHeadWeight(): {
  write: (serverName: string, weight: number) => void;
} {
  const { client } = useClient();
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const write = useCallback(
    (serverName: string, weight: number) => {
      if (!client) return;
      if (timerRef.current) clearTimeout(timerRef.current);
      timerRef.current = setTimeout(async () => {
        await writeServerConfig(client, serverName, (s) => ({
          ...s,
          weight,
        }));
      }, 300);
    },
    [client]
  );

  return { write };
}

// Debounced write — for leaf weight sliders.
export function useDebouncedLeafWeight(): {
  write: (serverName: string, leafSlug: string, weight: number, heads: HeadInfo[]) => void;
} {
  const { client } = useClient();
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const write = useCallback(
    (serverName: string, leafSlug: string, weight: number, heads: HeadInfo[]) => {
      if (!client) return;
      if (timerRef.current) clearTimeout(timerRef.current);
      timerRef.current = setTimeout(async () => {
        const head = heads.find((h) => h.name === serverName);
        if (!head) return;

        const weights: Record<string, number> = {};
        for (const leaf of head.leafs) {
          weights[leaf.slug] = leaf.slug === leafSlug ? weight : leaf.effective_weight;
        }
        const enabledSlugs = head.leafs.filter((l) => l.enabled).map((l) => l.slug);

        await writeServerConfig(client, serverName, (s) => ({
          ...s,
          leaf_preferences: {
            mode: enabledSlugs.length === head.leafs.length ? "ALL" : "SPECIFIC",
            weights,
            enabled: enabledSlugs.length === head.leafs.length ? undefined : enabledSlugs,
          },
        }));
      }, 300);
    },
    [client]
  );

  return { write };
}
