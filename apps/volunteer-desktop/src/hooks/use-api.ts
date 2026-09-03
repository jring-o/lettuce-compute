import { useCallback, useEffect, useRef, useState } from "react";
import { ManagementClient } from "../api/client";

let clientPromise: Promise<ManagementClient> | null = null;
let clientInstance: ManagementClient | null = null;

/**
 * The management-API client, connecting in the background. While the daemon
 * is not answering (`ManagementClient.create` rejects — not running, or still
 * starting) `error` is set and the connection is retried every 2 s; once a
 * retry succeeds `client` is set AND `error` is cleared. The clearing matters:
 * a hook mounted during the daemon's start (the layout's status bar, at app
 * launch) would otherwise keep a stale error beside a live client and never
 * start polling, reading "Stopped" until the window was reloaded (TB-55).
 */
export function useClient(): {
  client: ManagementClient | null;
  error: Error | null;
} {
  const [client, setClient] = useState<ManagementClient | null>(clientInstance);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    if (clientInstance) {
      setClient(clientInstance);
      return;
    }

    let cancelled = false;

    const tryConnect = () => {
      if (!clientPromise) {
        clientPromise = ManagementClient.create();
      }

      clientPromise
        .then((c) => {
          clientInstance = c;
          if (!cancelled) {
            setClient(c);
            setError(null);
          }
        })
        .catch((err) => {
          clientPromise = null;
          if (!cancelled) {
            setError(err);
            // Retry in 2 seconds — daemon may still be starting
            setTimeout(() => {
              if (!cancelled) tryConnect();
            }, 2000);
          }
        });
    };

    tryConnect();
    return () => { cancelled = true; };
  }, []);

  return { client, error };
}

export function useApiQuery<T>(
  fetcher: (client: ManagementClient) => Promise<T>,
  intervalMs?: number
): {
  data: T | null;
  isLoading: boolean;
  error: Error | null;
  refetch: () => void;
} {
  const { client, error: clientError } = useClient();
  const [data, setData] = useState<T | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  const fetch = useCallback(async () => {
    if (!client) return;
    try {
      const result = await fetcherRef.current(client);
      setData(result);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)));
    } finally {
      setIsLoading(false);
    }
  }, [client]);

  useEffect(() => {
    // A connection error is only the answer while there is no client; once
    // one exists the query runs against it whatever the last attempt said.
    if (clientError && !client) {
      setError(clientError);
      setIsLoading(false);
      return;
    }

    fetch();

    if (intervalMs && intervalMs > 0) {
      const id = setInterval(fetch, intervalMs);
      return () => clearInterval(id);
    }
  }, [fetch, intervalMs, client, clientError]);

  return { data, isLoading, error, refetch: fetch };
}
