import { useCallback, useRef, useState, useEffect } from "react";
import { useClient } from "./use-api";
import { markRestartRequired, useOnDaemonRestart } from "./use-restart-required";
import { formatBytes } from "../lib/utils";
import type {
  ConfigResponse,
  ConfigUpdateResponse,
  HeadInfo,
  LeafPreferences,
  MachineCapabilities,
  ServerConfig,
} from "../api/client";

/** The fields that identify a head across the heads response and config.yaml. */
export type HeadRef = Pick<HeadInfo, "name" | "grpc_address">;

/** Head gRPC address -> its `trusted_runtimes` (uppercase). Absent when not known. */
export type TrustByHead = Record<string, string[]>;

/**
 * The config entry for a head. `GET /api/v1/heads` reports `name` as the
 * head's own display title (for example "LBRY.Science - Lettuce Rip"), while
 * config.yaml stores the alias the CLI recorded at attach time ("lbry.science"),
 * so the two agree only by accident; the gRPC address is the identity both
 * sides share. The name is a fallback for a config entry without an address.
 */
export function findServerConfig(servers: ServerConfig[], head: HeadRef): ServerConfig | undefined {
  return (
    servers.find((s) => s.grpc_address !== "" && s.grpc_address === head.grpc_address) ??
    servers.find((s) => s.name === head.name)
  );
}

/**
 * The `name` the daemon matches a `servers[]` update against: the config
 * entry's alias, or its address when it has no alias (the daemon's own
 * DisplayName rule). Throws when the head has no config entry, because the
 * daemon silently ignores an unknown name and the write would look saved.
 */
export function resolveServerName(servers: ServerConfig[], head: HeadRef): string {
  const server = findServerConfig(servers, head);
  if (!server) {
    throw new Error(`${head.name} (${head.grpc_address}) is not in the daemon's configuration`);
  }
  return server.name || server.grpc_address;
}

/** Map each head (by gRPC address) to the `trusted_runtimes` of its config entry. */
export function trustByHeadFromConfig(heads: HeadInfo[], servers: ServerConfig[]): TrustByHead {
  const out: TrustByHead = {};
  for (const head of heads) {
    const server = findServerConfig(servers, head);
    if (server) out[head.grpc_address] = server.trusted_runtimes.map((r) => r.toUpperCase());
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

/**
 * Read the current config, apply a change to the head's own entry (found by
 * gRPC address, see findServerConfig) and write the whole server list back.
 * Rejects when the head has no config entry rather than writing nothing.
 */
async function writeServerConfig(
  client: { config: () => Promise<ConfigResponse>; updateConfig: (partial: Record<string, unknown>) => Promise<unknown> },
  head: HeadRef,
  apply: (server: ServerConfig) => ServerConfig
) {
  const config = await client.config();
  const target = findServerConfig(config.servers, head);
  if (!target) {
    throw new Error(`${head.name} (${head.grpc_address}) is not in the daemon's configuration`);
  }
  const servers = config.servers.map((s) => (s === target ? apply(s) : s));
  await client.updateConfig({ servers });
}

// Immediate write — for discrete actions (checkbox toggle).
export function useWriteLeafPreferences(): {
  write: (head: HeadRef, prefs: LeafPreferences) => Promise<void>;
} {
  const { client } = useClient();

  const write = useCallback(
    async (head: HeadRef, prefs: LeafPreferences) => {
      if (!client) return;
      await writeServerConfig(client, head, (s) => ({
        ...s,
        leaf_preferences: prefs,
      }));
    },
    [client]
  );

  return { write };
}

/**
 * Set a head's `trusted_runtimes` exactly. Reads the config once to find the
 * head's entry and sends only that entry's name and the new list —
 * `PUT /api/v1/config` merges per-head fields by name — returning the
 * daemon's answer. Resolves to null when there is no client yet.
 *
 * Runtime trust is read when the daemon starts (the per-head clients are
 * built then), so the change is recorded as waiting for a restart unless
 * the daemon explicitly answers `restart_required: false`. An older daemon
 * that does not report the field at all still needs the restart.
 */
export function useWriteHeadTrust(): {
  write: (head: HeadRef, trustedRuntimes: string[]) => Promise<ConfigUpdateResponse | null>;
} {
  const { client } = useClient();

  const write = useCallback(
    async (head: HeadRef, trustedRuntimes: string[]) => {
      if (!client) return null;
      const config = await client.config();
      const name = resolveServerName(config.servers, head);
      const resp = await client.updateConfig({
        servers: [{ name, trusted_runtimes: trustedRuntimes }],
      });
      if (resp?.restart_required !== false) {
        markRestartRequired(
          `Trust settings for ${head.name} are saved. Lettuce applies them the next time it starts.`
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

/**
 * Raise `resource_limits.max_memory_mb` to a Memory-slider stop, the same
 * way the disk raise works. Unlike the disk gate, the memory ceiling is
 * advertised to each head when the daemon registers, and a head only offers
 * work whose declared memory fits the figure it holds — so a restart is
 * always recorded; until it happens the head keeps refusing the leaf (TB-66).
 */
export function useRaiseMemoryAllowance(): {
  raise: (mb: number) => Promise<ConfigUpdateResponse | null>;
} {
  const { client } = useClient();

  const raise = useCallback(
    async (mb: number) => {
      if (!client) return null;
      const config = await client.config();
      const resp = await client.updateConfig({
        resource_limits: { ...config.resource_limits, max_memory_mb: mb },
      });
      markRestartRequired(
        `Your memory allowance is now ${formatBytes(mb)}. Lettuce tells its servers the new figure the next time it starts; until then they keep offering only work that fit the old one.`
      );
      return resp;
    },
    [client]
  );

  return { raise };
}

// Debounced write — for continuous inputs (sliders).
export function useDebouncedHeadWeight(): {
  write: (head: HeadRef, weight: number) => void;
} {
  const { client } = useClient();
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const write = useCallback(
    (head: HeadRef, weight: number) => {
      if (!client) return;
      if (timerRef.current) clearTimeout(timerRef.current);
      timerRef.current = setTimeout(async () => {
        await writeServerConfig(client, head, (s) => ({
          ...s,
          weight,
        }));
      }, 300);
    },
    [client]
  );

  return { write };
}

// Debounced write — for leaf weight sliders. `head` carries the leaf list
// the weights and enabled set are derived from.
export function useDebouncedLeafWeight(): {
  write: (head: HeadInfo, leafSlug: string, weight: number) => void;
} {
  const { client } = useClient();
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const write = useCallback(
    (head: HeadInfo, leafSlug: string, weight: number) => {
      if (!client) return;
      if (timerRef.current) clearTimeout(timerRef.current);
      timerRef.current = setTimeout(async () => {
        const weights: Record<string, number> = {};
        for (const leaf of head.leafs) {
          weights[leaf.slug] = leaf.slug === leafSlug ? weight : leaf.effective_weight;
        }
        const enabledSlugs = head.leafs.filter((l) => l.enabled).map((l) => l.slug);

        await writeServerConfig(client, head, (s) => ({
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
