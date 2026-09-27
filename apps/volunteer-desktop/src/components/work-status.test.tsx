import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { HeadCredit, LeafWorkStatus } from "@/api/client";
import { WorkStatusByHead, PENDING_NOTE } from "./work-status";

function leaf(overrides: Partial<LeafWorkStatus>): LeafWorkStatus {
  return {
    leaf_id: "leaf-1",
    leaf_name: "Leaf",
    results_pending: 0,
    results_agreed: 0,
    results_disagreed: 0,
    results_awaiting_content_verification: 0,
    results_content_verification_failed: 0,
    results_superseded: 0,
    runs_stopped: 0,
    copies_running: 0,
    copies_waiting_to_start: 0,
    ...overrides,
  };
}

function head(name: string, overrides: Partial<HeadCredit>): HeadCredit {
  return { head_name: name, volunteer_id: "vol-1", total_credit: 1, available: true, ...overrides };
}

describe("WorkStatusByHead", () => {
  it("lists each leaf's results by state and copies in progress, without zero lines", () => {
    render(
      <WorkStatusByHead
        heads={[
          head("scios", {
            work_status: {
              by_leaf: [
                leaf({
                  leaf_id: "gpu",
                  leaf_name: "GPU leaf",
                  results_pending: 523,
                  results_agreed: 10,
                  results_superseded: 4,
                  runs_stopped: 5,
                  copies_running: 2,
                  copies_waiting_to_start: 3,
                }),
              ],
            },
          }),
        ]}
      />
    );

    expect(screen.getByText("scios")).toBeInTheDocument();
    expect(screen.getByText("GPU leaf")).toBeInTheDocument();
    const row = (label: string) => screen.getByText(label).parentElement as HTMLElement;
    expect(within(row("Waiting for validation")).getByText("523")).toBeInTheDocument();
    expect(within(row("Agreed (credited)")).getByText("10")).toBeInTheDocument();
    expect(within(row("Not compared (work unit retired)")).getByText("4")).toBeInTheDocument();
    expect(within(row("Stopped: enough results arrived while it ran")).getByText("5")).toBeInTheDocument();
    expect(screen.getByText("2 running, 3 waiting to start")).toBeInTheDocument();
    expect(screen.queryByText("Did not agree")).not.toBeInTheDocument();
    expect(screen.queryByText("Checking the uploaded output")).not.toBeInTheDocument();
    expect(screen.getByText(PENDING_NOTE)).toBeInTheDocument();
  });

  it("says when a head does not report the figures, and when it has nothing yet", () => {
    render(
      <WorkStatusByHead
        heads={[
          head("old-head", {}),
          head("null-head", { work_status: null }),
          head("new-head", { work_status: { by_leaf: [] } }),
          head("down-head", { available: false }),
        ]}
      />
    );

    expect(screen.getAllByText("Not reported by this head (it runs an older version)")).toHaveLength(2);
    expect(screen.getByText("No results or copies on this head yet")).toBeInTheDocument();
    expect(screen.queryByText("down-head")).not.toBeInTheDocument();
    expect(screen.queryByText("0")).not.toBeInTheDocument();
    expect(screen.queryByText(PENDING_NOTE)).not.toBeInTheDocument();
  });

  it("renders nothing when no head answered", () => {
    const { container } = render(<WorkStatusByHead heads={[head("down-head", { available: false })]} />);
    expect(container).toBeEmptyDOMElement();
  });
});
