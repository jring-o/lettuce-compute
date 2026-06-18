import type {
  AggregationResult,
  ArtifactVersion,
  CreateLeafRequest,
  GenerateWorkUnitsRequest,
  GenerateWorkUnitsResponse,
  HealthResponse,
  Leaf,
  LeafStats,
  LeafSummary,
  ListLeafsParams,
  ListResultsParams,
  ListWorkUnitsParams,
  PaginatedResponse,
  PublishVersionRequest,
  Result,
  StatsHistoryParams,
  UpdateLeafRequest,
  WorkUnit,
  WorkUnitSummary,
} from "@/types/infrastructure";

export class InfrastructureApiError extends Error {
  constructor(
    public code: string,
    message: string,
    public status: number,
    public details?: Record<string, unknown>,
  ) {
    super(message);
    this.name = "InfrastructureApiError";
  }
}

function buildQueryString(params: object): string {
  const searchParams = new URLSearchParams();
  for (const [key, value] of Object.entries(params as Record<string, unknown>)) {
    if (value !== undefined && value !== null) {
      searchParams.set(key, String(value));
    }
  }
  const qs = searchParams.toString();
  return qs ? `?${qs}` : "";
}

export class InfrastructureClient {
  private baseUrl: string;
  private apiKey: string | undefined;

  constructor(baseUrl: string, apiKey?: string) {
    // Strip trailing slash
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    this.apiKey = apiKey;
  }

  private async request<T>(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<T> {
    const url = `${this.baseUrl}${path}`;
    const headers: Record<string, string> = {};
    if (this.apiKey) {
      headers["Authorization"] = `Bearer ${this.apiKey}`;
    }
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
    }

    const res = await fetch(url, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(30_000),
    });

    if (res.status === 204) {
      return undefined as T;
    }

    const json = await res.json();

    if (!res.ok) {
      const err = json?.error ?? {};
      throw new InfrastructureApiError(
        err.code ?? "UNKNOWN_ERROR",
        err.message ?? `Infrastructure API error: ${res.status}`,
        res.status,
        err.details,
      );
    }

    return json as T;
  }

  // --- Health ---

  async getHealth(): Promise<HealthResponse> {
    return this.request("GET", "/api/v1/health/detailed");
  }

  // --- Leafs ---

  async createLeaf(data: CreateLeafRequest): Promise<Leaf> {
    return this.request("POST", "/api/v1/leafs", data);
  }

  async getLeaf(leafId: string): Promise<Leaf> {
    return this.request("GET", `/api/v1/leafs/${leafId}`);
  }

  async listLeafs(
    params?: ListLeafsParams,
  ): Promise<PaginatedResponse<LeafSummary>> {
    const qs = params ? buildQueryString(params) : "";
    return this.request("GET", `/api/v1/leafs${qs}`);
  }

  async updateLeaf(
    leafId: string,
    data: UpdateLeafRequest,
  ): Promise<Leaf> {
    return this.request("PUT", `/api/v1/leafs/${leafId}`, data);
  }

  async deleteLeaf(leafId: string): Promise<void> {
    return this.request("DELETE", `/api/v1/leafs/${leafId}`);
  }

  // --- State Transitions ---

  async activateLeaf(leafId: string): Promise<Leaf> {
    return this.request("POST", `/api/v1/leafs/${leafId}/activate`);
  }

  async pauseLeaf(leafId: string): Promise<Leaf> {
    return this.request("POST", `/api/v1/leafs/${leafId}/pause`);
  }

  async resumeLeaf(leafId: string): Promise<Leaf> {
    return this.request("POST", `/api/v1/leafs/${leafId}/resume`);
  }

  async archiveLeaf(leafId: string): Promise<Leaf> {
    return this.request("POST", `/api/v1/leafs/${leafId}/archive`);
  }

  async configureLeaf(leafId: string): Promise<Leaf> {
    return this.request("POST", `/api/v1/leafs/${leafId}/configure`);
  }

  // --- Artifact Versions (TODO #38) ---

  async listVersions(leafId: string): Promise<{ data: ArtifactVersion[] }> {
    return this.request("GET", `/api/v1/leafs/${leafId}/versions`);
  }

  async publishVersion(
    leafId: string,
    data: PublishVersionRequest,
  ): Promise<ArtifactVersion> {
    return this.request("POST", `/api/v1/leafs/${leafId}/versions`, data);
  }

  async activateVersion(
    leafId: string,
    versionId: string,
  ): Promise<ArtifactVersion> {
    return this.request(
      "POST",
      `/api/v1/leafs/${leafId}/versions/${versionId}/activate`,
    );
  }

  async deleteVersion(leafId: string, versionId: string): Promise<void> {
    return this.request(
      "DELETE",
      `/api/v1/leafs/${leafId}/versions/${versionId}`,
    );
  }

  // --- Work Units ---

  async listWorkUnits(
    leafId: string,
    params?: ListWorkUnitsParams,
  ): Promise<PaginatedResponse<WorkUnitSummary>> {
    const qs = params ? buildQueryString(params) : "";
    return this.request(
      "GET",
      `/api/v1/leafs/${leafId}/work-units${qs}`,
    );
  }

  async getWorkUnit(
    leafId: string,
    workUnitId: string,
  ): Promise<WorkUnit> {
    return this.request(
      "GET",
      `/api/v1/leafs/${leafId}/work-units/${workUnitId}`,
    );
  }

  async generateWorkUnits(
    leafId: string,
    data?: GenerateWorkUnitsRequest,
  ): Promise<GenerateWorkUnitsResponse> {
    return this.request(
      "POST",
      `/api/v1/leafs/${leafId}/work-units/generate`,
      data,
    );
  }

  // --- Results ---

  async listResults(
    leafId: string,
    params?: ListResultsParams,
  ): Promise<PaginatedResponse<Result>> {
    const qs = params ? buildQueryString(params) : "";
    return this.request(
      "GET",
      `/api/v1/leafs/${leafId}/results${qs}`,
    );
  }

  // --- Statistics ---

  async getLeafStats(leafId: string): Promise<LeafStats> {
    return this.request("GET", `/api/v1/leafs/${leafId}/stats`);
  }

  async getLeafStatsBatch(
    leafIds: string[],
  ): Promise<Record<string, LeafStats>> {
    const ids = leafIds.join(",");
    const resp = await this.request<{ data: Record<string, LeafStats> }>(
      "GET",
      `/api/v1/leafs/stats/batch?ids=${ids}`,
    );
    return resp.data;
  }

  async getLeafStatsHistory(
    leafId: string,
    params: StatsHistoryParams,
  ): Promise<{ data: LeafStats[] }> {
    const qs = buildQueryString(params);
    return this.request(
      "GET",
      `/api/v1/leafs/${leafId}/stats/history${qs}`,
    );
  }

  // --- Aggregation ---

  async triggerAggregation(
    leafId: string,
    options?: {
      batchId?: string;
      format?: "json" | "csv";
      force?: boolean;
    },
  ): Promise<{ data: AggregationResult }> {
    return this.request(
      "POST",
      `/api/v1/leafs/${leafId}/aggregate`,
      options,
    );
  }

  async getAggregation(
    leafId: string,
  ): Promise<{ data: AggregationResult } | null> {
    try {
      return await this.request(
        "GET",
        `/api/v1/leafs/${leafId}/aggregate`,
      );
    } catch (err) {
      if (err instanceof InfrastructureApiError && err.status === 404) {
        return null;
      }
      throw err;
    }
  }

}

// Singleton instance — uses DASHBOARD_API_KEY for admin-level access to infrastructure.
export const infrastructureClient = new InfrastructureClient(
  process.env.INFRASTRUCTURE_API_URL ?? "http://localhost:8080",
  process.env.DASHBOARD_API_KEY,
);
