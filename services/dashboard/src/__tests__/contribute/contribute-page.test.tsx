import { render, screen } from "@testing-library/react";

// Mock all volunteer library modules before importing the component
jest.mock("@/lib/volunteer/identity", () => ({
  getOrCreateIdentity: jest.fn().mockResolvedValue({
    publicKey: new Uint8Array(32),
    privateKey: {},
    publicKeyBase64url: "mock-pub-key",
    fingerprint: "ab:cd:ef:12:34",
  }),
  createNewIdentity: jest.fn().mockResolvedValue({
    publicKey: new Uint8Array(32),
    privateKey: {},
    publicKeyBase64url: "mock-new-key",
    fingerprint: "56:78:90:ab:cd",
  }),
  deleteIdentity: jest.fn().mockResolvedValue(undefined),
}));

jest.mock("@/lib/volunteer/client", () => ({
  createVolunteerClient: jest.fn(() => ({
    register: jest.fn(),
  })),
}));

jest.mock("@/lib/volunteer/pool-manager", () => ({
  PoolManager: jest.fn(() => ({
    start: jest.fn(),
    stop: jest.fn(),
    setWorkerCount: jest.fn(),
  })),
}));

jest.mock("@/lib/volunteer/webgpu-dispatch", () => ({
  isWebGPUAvailable: jest.fn().mockResolvedValue(false),
}));

// Mock fetch for the LeafSelector
global.fetch = jest.fn().mockResolvedValue({
  ok: true,
  json: () => Promise.resolve({ name: "Test Head", leafs: [] }),
});

import { ContributePage } from "@/components/contribute/contribute-page";

describe("ContributePage", () => {
  beforeEach(() => {
    jest.clearAllMocks();
    Object.defineProperty(navigator, "hardwareConcurrency", {
      value: 8,
      configurable: true,
    });
  });

  it("renders the page heading", () => {
    render(<ContributePage />);
    expect(screen.getByText("Contribute Compute")).toBeInTheDocument();
  });

  it("renders the description text", () => {
    render(<ContributePage />);
    expect(
      screen.getByText(/Donate spare compute from your browser/)
    ).toBeInTheDocument();
  });

  it("renders the Hardware panel", () => {
    render(<ContributePage />);
    expect(screen.getByText("Hardware")).toBeInTheDocument();
  });

  it("renders the Identity card", () => {
    render(<ContributePage />);
    expect(screen.getByText("Identity")).toBeInTheDocument();
  });

  it("renders the Available Leafs card", () => {
    render(<ContributePage />);
    expect(screen.getByText("Available Leafs")).toBeInTheDocument();
  });

  it("renders the Controls card", () => {
    render(<ContributePage />);
    expect(screen.getByText("Controls")).toBeInTheDocument();
  });

  it("renders the Start Contributing button", () => {
    render(<ContributePage />);
    expect(
      screen.getByRole("button", { name: "Start Contributing" })
    ).toBeInTheDocument();
  });

  it("start button is disabled when no leafs are selected", () => {
    render(<ContributePage />);
    const btn = screen.getByRole("button", { name: "Start Contributing" });
    expect(btn).toBeDisabled();
  });

  it("renders the worker count slider", () => {
    render(<ContributePage />);
    expect(screen.getByLabelText(/Workers:/)).toBeInTheDocument();
  });
});
