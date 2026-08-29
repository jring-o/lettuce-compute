import { useCallback, useRef, useState, useEffect } from "react";
import { useClient } from "./use-api";
import { markRestartRequired, useOnDaemonRestart } from "./use-restart-required";
import type {
  ConfigResponse,
  ConfigUpdateResponse,
  HeadInfo,
  LeafPreferences,
  MachineCapabilities,
  ServerConfig,
} from "../api/client";

/** Head name -> its `trusted_runtimes` (uppercase). Absent when not known. */
export type TrustByHead = Record<string, string[]>;

/**
 * Map each head to the `trusted_runtimes` of its config entry. Entries are
 * matched by gRPC address first (the stable identity), then by name, which
 * is how the daemon itself matches a `servers[]` update.
 */
export function trustByHeadFromConfig(heads: HeadInfo[], servers: ServerConfig[]): TrustByHead {
  const out: TrustByHead = {};
  for (const head of heads) {
    const server =
      servers.find((s) => s.grpc_address === head.grpc_address) ??
      servers.find((s) => s.name === head.name);
    if (server) out[head.name] = server.trusted_runtimes.map((r) => r.toUpperCase());
  }
  return out;
}

export function useHeads(): {
  heads: HeadInfo[];
  /** This machine's capabilities as the running daemon sees them; null until loaded. */
  machine: MachineCapabilities | null;
  trustByHead: TrustByHead;
  isLoading: boolean;
  error: Error | null;
  refetch: () => void;
  setHeads: React.Dispatch<React.SetStateAction<HeadInfo[]>>;
  setTrustByHead: React.Dispatch<React.SetStateAction<TrustByHead>>;
} {
  const { client } = useClient();
  const [heads, setHeads] = useState<HeadInfo[]>([]);
  const [machine, setMachine] = useState<MachineCapabilities | null>(null);
  const [trustByHead, setTrustByHead] = useState<TrustByHead>({});
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const fetchedRef = useRef(false);

  const fetchHeads = useCallback(async () => {
    if (!client) return;
    try {
      const result = await client.headsAndMachine();
      setHeads(result.heads);
      setMachine(result.machine);
      setError(null);
      // Trust lives in config, not in the heads response. A config failure
      // leaves trust unknown rather than hiding the heads.
      try {
        const config = await client.config();
        setTrustByHead(trustByHeadFromConfig(result.heads, config.servers));
      } catch {
        setTrustByHead({});
      }
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

  // A restarted daemon reconnects to its heads and applies saved trust and
  // limits; show what it now reports.
  useOnDaemonRestart(fetchHeads);

  return {
    heads,
    machine,
    trustByHead,
    isLoading,
    error,
    refetch: fetchHeads,
    setHeads,
    setTrustByHead,
  };
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

/**
 * Set a head's `trusted_runtimes` exactly. Sends only the name and the new
 * list — `PUT /api/v1/config` merges per-head fields by name — and returns
 * the daemon's answer. Resolves to null when there is no client yet.
 *
 * Runtime trust is read when the daemon starts (the per-head clients are
 * built then), so the change is recorded as waiting for a restart unless
 * the daemon explicitly answers `restart_required: false`. An older daemon
 * that does not report the field at all still needs the restart.
 */
export function useWriteHeadTrust(): {
  write: (serverName: string, trustedRuntimes: string[]) => Promise<ConfigUpdateResponse | null>;
} {
  const { client } = useClient();

  const write = useCallback(
    async (serverName: string, trustedRuntimes: string[]) => {
      if (!client) return null;
      const resp = await client.updateConfig({
        servers: [{ name: serverName, trusted_runtimes: trustedRuntimes }],
      });
      if (resp?.restart_required !== false) {
        markRestartRequired(
          `Trust settings for ${serverName} are saved. Lettuce applies them the next time it starts.`
        );
      }
      return resp;
    },
    [client]
  );

  return { write };
}

/**
 * Raise `resource_limits.max_disk_gb`. Reads the current limits first and
 * sends the whole object with the new figure, as the settings page does, so
 * no other limit is disturbed. Returns the daemon's answer; null when there
 * is no client yet. The disk gate reads the allowance live, so a restart is
 * recorded only when the daemon says one is needed.
 */
export function useRaiseDiskAllowance(): {
  raise: (gb: number) => Promise<ConfigUpdateResponse | null>;
} {
  const { client } = useClient();

  const raise = useCallback(
    async (gb: number) => {
      if (!client) return null;
      const config = await client.config();
      const resp = await client.updateConfig({
        resource_limits: { ...config.resource_limits, max_disk_gb: gb },
      });
      if (resp?.restart_required === true) {
        markRestartRequired(
          `Your disk allowance is now ${gb} GB. Lettuce applies it the next time it starts.`
        );
      }
      return resp;
    },
    [client]
  );

  return { raise };
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
