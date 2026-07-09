import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { PowSolveCancelledError } from "@/lib/volunteer/pow-solver";
import type { RegisterFlowHooks } from "@/lib/volunteer/register-flow";

// Exercises the page's proof-of-work wiring: the "solving" state, the cancel button,
// and the unmount teardown. The flow itself is pinned by register-flow.test.ts, so it
// is mocked here; PowSolveCancelledError stays REAL because the page classifies the
// cancel rejection with instanceof.

const mockRegisterWithPow = jest.fn();

jest.mock("@/lib/volunteer/register-flow", () => ({
  registerWithPow: (...args: unknown[]) => mockRegisterWithPow(...args),
}));

jest.mock("@/lib/volunteer/identity", () => ({
  getOrCreateIdentity: jest.fn().mockResolvedValue({
    publicKey: new Uint8Array(32),
    privateKey: {},
    publicKeyBase64url: "mock-pub-key",
    fingerprint: "ab:cd:ef:12:34",
  }),
  createNewIdentity: jest.fn(),
  deleteIdentity: jest.fn(),
}));

jest.mock("@/lib/volunteer/pool-manager", () => ({
  PoolManager: jest.fn(() => ({
    start: jest.fn().mockResolvedValue(undefined),
    stop: jest.fn().mockResolvedValue(undefined),
    setWorkerCount: jest.fn(),
  })),
}));

jest.mock("@/lib/volunteer/webgpu-dispatch", () => ({
  isWebGPUAvailable: jest.fn().mockResolvedValue(false),
}));

// One ACTIVE WASM leaf so a leaf can be selected and Start enabled.
global.fetch = jest.fn().mockResolvedValue({
  ok: true,
  json: () =>
    Promise.resolve({
      name: "Test Head",
      leafs: [
        {
          id: "leaf-1",
          name: "Leaf One",
          description: "",
          state: "ACTIVE",
          queued_work_units: 3,
          execution_spec: { binaries: { wasm: "https://example.test/a.wasm" } },
        },
      ],
    }),
});

import { ContributePage } from "@/components/contribute/contribute-page";

// Renders, selects the leaf, and clicks Start Contributing.
async function startContributing() {
  const view = render(<ContributePage />);
  const leafSwitch = await screen.findByRole("switch", {
    name: "Enable Leaf One",
  });
  fireEvent.click(leafSwitch);
  const start = screen.getByRole("button", { name: "Start Contributing" });
  await waitFor(() => expect(start).toBeEnabled());
  fireEvent.click(start);
  return view;
}

// A flow stub that immediately reports solving and only settles via cancel().
function pendingSolvingFlow() {
  let rejectFn!: (err: Error) => void;
  const cancel = jest.fn(() => rejectFn(new PowSolveCancelledError()));
  mockRegisterWithPow.mockImplementation(
    (_client, _hardware, _publicKey, hooks: RegisterFlowHooks) => {
      hooks.onSolvingStart?.(20);
      return {
        promise: new Promise((_resolve, reject) => {
          rejectFn = reject;
        }),
        cancel,
      };
    }
  );
  return { cancel };
}

describe("ContributePage proof-of-work wiring", () => {
  beforeEach(() => {
    jest.clearAllMocks();
    Object.defineProperty(navigator, "hardwareConcurrency", {
      value: 8,
      configurable: true,
    });
  });

  it("reaches running through the register flow when no solve is required", async () => {
    mockRegisterWithPow.mockImplementation(() => ({
      promise: Promise.resolve({
        volunteer_id: "vol-1",
        registered_at: "2026-07-09T00:00:00Z",
      }),
      cancel: jest.fn(),
    }));

    await startContributing();

    await screen.findByRole("button", { name: "Stop Contributing" });
    expect(screen.getByText("Registered successfully")).toBeInTheDocument();
    expect(mockRegisterWithPow).toHaveBeenCalledTimes(1);
    // The registering identity's key rides the flow (the solve preimage binds it).
    expect(mockRegisterWithPow.mock.calls[0][2]).toBe("mock-pub-key");
  });

  it("shows the cancel button while solving and cancel returns to idle", async () => {
    const { cancel } = pendingSolvingFlow();

    await startContributing();

    const cancelBtn = await screen.findByRole("button", {
      name: "Solving challenge... — Cancel",
    });
    expect(
      screen.getByText(/solving a difficulty-20 challenge/)
    ).toBeInTheDocument();

    fireEvent.click(cancelBtn);
    expect(cancel).toHaveBeenCalledTimes(1);

    await screen.findByText("Registration cancelled");
    expect(
      screen.getByRole("button", { name: "Start Contributing" })
    ).toBeInTheDocument();
  });

  it("cancels an in-flight solve on unmount", async () => {
    const { cancel } = pendingSolvingFlow();

    const view = await startContributing();
    await screen.findByRole("button", { name: "Solving challenge... — Cancel" });

    view.unmount();
    expect(cancel).toHaveBeenCalledTimes(1);
  });
});
