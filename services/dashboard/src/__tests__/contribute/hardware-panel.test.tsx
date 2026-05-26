import { render, screen, waitFor } from "@testing-library/react";

// Mock webgpu-dispatch before importing the component
jest.mock("@/lib/volunteer/webgpu-dispatch", () => ({
  isWebGPUAvailable: jest.fn(),
}));

import { HardwarePanel } from "@/components/contribute/hardware-panel";
import { isWebGPUAvailable } from "@/lib/volunteer/webgpu-dispatch";

const mockIsWebGPUAvailable = isWebGPUAvailable as jest.MockedFunction<
  typeof isWebGPUAvailable
>;

describe("HardwarePanel", () => {
  beforeEach(() => {
    jest.clearAllMocks();
    // Default: WebGPU not available
    mockIsWebGPUAvailable.mockResolvedValue(false);

    // Mock navigator.hardwareConcurrency
    Object.defineProperty(navigator, "hardwareConcurrency", {
      value: 8,
      configurable: true,
    });
  });

  it("shows 'Detecting...' initially before hardware detection completes", () => {
    // Make the detection hang so we see the loading state
    mockIsWebGPUAvailable.mockReturnValue(new Promise(() => {}));
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    expect(screen.getByText("Hardware")).toBeInTheDocument();
    expect(screen.getByText("Detecting...")).toBeInTheDocument();
  });

  it("renders detected CPU cores after detection", async () => {
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText("CPU Cores")).toBeInTheDocument();
    });
    expect(screen.getByText("8")).toBeInTheDocument();
  });

  it("calls onDetected with hardware info", async () => {
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(onDetected).toHaveBeenCalledTimes(1);
    });

    const info = onDetected.mock.calls[0][0];
    expect(info.cpuCores).toBe(8);
    expect(info.webgpuAvailable).toBe(false);
    expect(info.wasmSupported).toBe(true);
    expect(typeof info.browser).toBe("string");
  });

  it("shows WebGPU as Not Available when not supported", async () => {
    mockIsWebGPUAvailable.mockResolvedValue(false);
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText("Not Available")).toBeInTheDocument();
    });
  });

  it("shows WebGPU as Available when supported", async () => {
    mockIsWebGPUAvailable.mockResolvedValue(true);
    // Mock navigator.gpu
    Object.defineProperty(navigator, "gpu", {
      value: {
        requestAdapter: jest.fn().mockResolvedValue(null),
      },
      configurable: true,
    });
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText(/Available/)).toBeInTheDocument();
    });
  });

  it("shows WASM as Supported when WebAssembly is available", async () => {
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText("Supported")).toBeInTheDocument();
    });
  });

  it("shows Memory as Unknown when deviceMemory is not available", async () => {
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText("Unknown")).toBeInTheDocument();
    });
  });

  it("shows Memory in GB when deviceMemory is available", async () => {
    Object.defineProperty(navigator, "deviceMemory", {
      value: 16,
      configurable: true,
    });
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText("16 GB")).toBeInTheDocument();
    });

    // Clean up
    Object.defineProperty(navigator, "deviceMemory", {
      value: undefined,
      configurable: true,
    });
  });

  it("summarizes Chrome user agent", async () => {
    Object.defineProperty(navigator, "userAgent", {
      value:
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.6099.130 Safari/537.36",
      configurable: true,
    });
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText("Chrome 120.0.6099.130")).toBeInTheDocument();
    });
  });

  it("summarizes Firefox user agent", async () => {
    Object.defineProperty(navigator, "userAgent", {
      value: "Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
      configurable: true,
    });
    const onDetected = jest.fn();

    render(<HardwarePanel onDetected={onDetected} />);

    await waitFor(() => {
      expect(screen.getByText("Firefox 121.0")).toBeInTheDocument();
    });
  });
});
