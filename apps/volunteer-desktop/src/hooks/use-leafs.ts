import { useCallback, useEffect, useRef, useState } from "react";
import { type AvailableLeaf, type SearchParams } from "../api/client";
import { useClient } from "./use-api";

export function useAvailableLeafs(params: SearchParams): {
  leafs: AvailableLeaf[];
  isLoading: boolean;
  error: Error | null;
} {
  const { client, error: clientError } = useClient();
  const [leafs, setLeafs] = useState<AvailableLeaf[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const paramsRef = useRef(params);
  paramsRef.current = params;

  const fetch = useCallback(async () => {
    if (!client) return;
    setIsLoading(true);
    try {
      const result = await client.availableLeafs(paramsRef.current);
      setLeafs(result);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)));
    } finally {
      setIsLoading(false);
    }
  }, [client]);

  useEffect(() => {
    if (clientError) {
      setError(clientError);
      setIsLoading(false);
      return;
    }
    fetch();
  }, [fetch, clientError, params.query, params.research_area]);

  return { leafs, isLoading, error };
}
