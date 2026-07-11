import { render, screen, waitFor, act, fireEvent } from "@testing-library/react";
import type { WorkUnitSummary, Result } from "@/types/infrastructure";

// Mock child components
jest.mock("@/components/visualization/VizIframe", () => ({
  VizIframe: (props: Record<string, unknown>) => (
    <div data-testid="viz-iframe" data-output={JSON.stringify(props.resultOutputData)} />
  ),
}));

jest.mock("@/components/visualization/WorkUnitSelector", () => ({
  WorkUnitSelector: (props: {
    workUnits: WorkUnitSummary[];
    selectedId: string | null;
    onSelect: (id: string) => void;
    loading?: boolean;
  }) => (
    <div data-testid="work-unit-selector">
      <select
        data-testid="wu-select"
        value={props.selectedId ?? ""}
        onChange={(e) => props.onSelect(e.target.value)}
      >
        {props.workUnits.map((wu) => (
          <option key={wu.id} value={wu.id}>
            {wu.id}
          </option>
        ))}
      </select>
      {props.loading && <span data-testid="selector-loading">Loading...</span>}
    </div>
  ),
}));

import { VisualizationPage } from "@/components/visualization/VisualizationPage";

function makeWorkUnit(
  overrides: Partial<WorkUnitSummary> = {},
): WorkUnitSummary {
  return {
    id: "wu-00000000-0000-0000-0000-000000000001",
    leaf_id: "leaf-1",
    batch_id: null,
    state: "VALIDATED",
    priority: "NORMAL",
    assigned_to: null,
    attempts: 1,
    flagged_for_review: false,
    created_at: "2026-03-15T10:00:00Z",
    updated_at: "2026-03-15T12:30:00Z",
    ...overrides,
  };
}

function makeResult(overrides: Partial<Result> = {}): Result {
  return {
    id: "result-1",
    work_unit_id: "wu-00000000-0000-0000-0000-000000000001",
    volunteer_id: "vol-1",
    output_data: { particles: [1, 2, 3] },
    output_checksum: "abc123",
    execution_metadata: {
      wall_clock_seconds: 10,
      cpu_seconds_user: 8,
      cpu_seconds_system: 1,
      cpu_cores_used: 2,
      gpu_seconds: 0,
      gpu_vram_used_mb: 0,
      peak_memory_mb: 256,
      disk_read_mb: 1,
      disk_write_mb: 0.5,
      network_rx_mb: 0,
      network_tx_mb: 0,
    },
    validation_status: "AGREED",
    submitted_at: "2026-03-15T12:00:00Z",
    created_at: "2026-03-15T12:00:00Z",
    updated_at: "2026-03-15T12:00:00Z",
    ...overrides,
  };
}

const defaultProps = {
  vizBundleUrl: "https://example.com/bundle.tar.gz",
  vizOrigin: "https://viz.example.com",
  platformUrl: "https://app.example.com",
  leafSlug: "nbody-sim",
  leafId: "leaf-1",
  workUnits: [
    makeWorkUnit({ id: "wu-aaa" }),
    makeWorkUnit({ id: "wu-bbb" }),
  ],
  initialResult: makeResult({ work_unit_id: "wu-aaa" }),
};

describe("VisualizationPage", () => {
  let fetchMock: jest.Mock;
  const originalFetch = global.fetch;

  beforeEach(() => {
    fetchMock = jest.fn().mockResolvedValue({
      json: () => Promise.resolve({ result: null }),
    });
    global.fetch = fetchMock;
  });

  afterEach(() => {
    global.fetch = originalFetch;
  });

  it("shows empty state when workUnits array is empty", () => {
    render(
      <VisualizationPage
        {...defaultProps}
        workUnits={[]}
        initialResult={null}
      />,
    );

    expect(
      screen.getByText(
        "No completed work units with visualization data available.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("viz-iframe")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("work-unit-selector"),
    ).not.toBeInTheDocument();
  });

  it("renders WorkUnitSelector and VizIframe when work units are provided", () => {
    render(<VisualizationPage {...defaultProps} />);

    expect(screen.getByTestId("work-unit-selector")).toBeInTheDocument();
    expect(screen.getByTestId("viz-iframe")).toBeInTheDocument();
  });

  it("passes initialResult output_data to VizIframe on first render without fetching", () => {
    render(<VisualizationPage {...defaultProps} />);

    const iframe = screen.getByTestId("viz-iframe");
    const outputData = JSON.parse(iframe.getAttribute("data-output")!);
    expect(outputData).toEqual({ particles: [1, 2, 3] });

    // Should not have fetched because the initial WU already has a result
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("fetches result when a different work unit is selected", async () => {
    const fetchedResult = makeResult({
      work_unit_id: "wu-bbb",
      output_data: { step: 99 },
    });

    fetchMock.mockResolvedValueOnce({
      json: () => Promise.resolve({ result: fetchedResult }),
    });

    render(<VisualizationPage {...defaultProps} />);

    // Select a different WU
    const select = screen.getByTestId("wu-select");
    await act(async () => {
      fireEvent.change(select, { target: { value: "wu-bbb" } });
    });

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/viz/results?leafId=leaf-1&workUnitId=wu-bbb",
      );
    });

    // After fetch resolves, VizIframe should get the new data
    await waitFor(() => {
      const iframe = screen.getByTestId("viz-iframe");
      const outputData = JSON.parse(iframe.getAttribute("data-output")!);
      expect(outputData).toEqual({ step: 99 });
    });
  });

  it("shows loading state during fetch", async () => {
    // Use a deferred promise to control when fetch resolves
    let resolveFetch!: (value: { json: () => Promise<unknown> }) => void;
    const fetchPromise = new Promise<{ json: () => Promise<unknown> }>((resolve) => {
      resolveFetch = resolve;
    });
    fetchMock.mockReturnValueOnce(fetchPromise);

    render(<VisualizationPage {...defaultProps} />);

    // Select a different WU to trigger fetch
    const select = screen.getByTestId("wu-select");
    await act(async () => {
      fireEvent.change(select, { target: { value: "wu-bbb" } });
    });

    // Loading state should be visible
    await waitFor(() => {
      expect(screen.getByText("Loading visualization data...")).toBeInTheDocument();
    });

    // Resolve the fetch
    await act(async () => {
      resolveFetch({
        json: () =>
          Promise.resolve({
            result: makeResult({ work_unit_id: "wu-bbb" }),
          }),
      });
    });

    // Loading state should be gone
    await waitFor(() => {
      expect(
        screen.queryByText("Loading visualization data..."),
      ).not.toBeInTheDocument();
    });
  });

  it("handles fetch error silently without crashing", async () => {
    fetchMock.mockRejectedValueOnce(new Error("Network error"));

    render(<VisualizationPage {...defaultProps} />);

    const select = screen.getByTestId("wu-select");
    await act(async () => {
      fireEvent.change(select, { target: { value: "wu-bbb" } });
    });

    // Should not crash — loading eventually ends
    await waitFor(() => {
      expect(
        screen.queryByText("Loading visualization data..."),
      ).not.toBeInTheDocument();
    });
  });

  it("does not fetch when initial result matches selected WU", () => {
    render(<VisualizationPage {...defaultProps} />);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("includes volunteerFilter in fetch URL when set", async () => {
    const fetchedResult = makeResult({
      work_unit_id: "wu-bbb",
      output_data: { filtered: true },
    });

    fetchMock.mockResolvedValueOnce({
      json: () => Promise.resolve({ result: fetchedResult }),
    });

    render(
      <VisualizationPage {...defaultProps} volunteerFilter="vol-test-123" />,
    );

    // Select a different WU to trigger a fetch
    const select = screen.getByTestId("wu-select");
    await act(async () => {
      fireEvent.change(select, { target: { value: "wu-bbb" } });
    });

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/viz/results?leafId=leaf-1&workUnitId=wu-bbb&volunteerId=vol-test-123",
      );
    });
  });
});
