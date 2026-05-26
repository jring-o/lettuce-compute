import { isWebGPUAvailable, dispatchGPUCompute, _resetDeviceCache } from "../webgpu-dispatch";

// Store original navigator to restore later.
const originalNavigator = global.navigator;

describe("WebGPU Dispatch", () => {
  afterEach(() => {
    // Restore navigator and reset device cache after each test.
    Object.defineProperty(global, "navigator", {
      value: originalNavigator,
      writable: true,
      configurable: true,
    });
    _resetDeviceCache();
  });

  describe("isWebGPUAvailable", () => {
    it("returns false when navigator.gpu is undefined", async () => {
      Object.defineProperty(global, "navigator", {
        value: { gpu: undefined },
        writable: true,
        configurable: true,
      });

      const result = await isWebGPUAvailable();
      expect(result).toBe(false);
    });

    it("returns false when navigator is undefined", async () => {
      Object.defineProperty(global, "navigator", {
        value: undefined,
        writable: true,
        configurable: true,
      });

      const result = await isWebGPUAvailable();
      expect(result).toBe(false);
    });

    it("returns false when requestAdapter returns null", async () => {
      Object.defineProperty(global, "navigator", {
        value: {
          gpu: {
            requestAdapter: jest.fn().mockResolvedValue(null),
          },
        },
        writable: true,
        configurable: true,
      });

      const result = await isWebGPUAvailable();
      expect(result).toBe(false);
    });

    it("returns true when adapter is available", async () => {
      Object.defineProperty(global, "navigator", {
        value: {
          gpu: {
            requestAdapter: jest.fn().mockResolvedValue({
              name: "Test GPU Adapter",
            }),
          },
        },
        writable: true,
        configurable: true,
      });

      const result = await isWebGPUAvailable();
      expect(result).toBe(true);
    });

    it("returns false when requestAdapter throws", async () => {
      Object.defineProperty(global, "navigator", {
        value: {
          gpu: {
            requestAdapter: jest
              .fn()
              .mockRejectedValue(new Error("GPU init failed")),
          },
        },
        writable: true,
        configurable: true,
      });

      const result = await isWebGPUAvailable();
      expect(result).toBe(false);
    });
  });

  describe("dispatchGPUCompute", () => {
    it("throws when navigator.gpu is not available", async () => {
      Object.defineProperty(global, "navigator", {
        value: { gpu: undefined },
        writable: true,
        configurable: true,
      });

      await expect(
        dispatchGPUCompute({
          wgslShaderUrl: "http://example.com/shader.wgsl",
          inputData: new Uint8Array([1, 2, 3]),
          outputSize: 12,
        })
      ).rejects.toThrow("WebGPU is not available");
    });

    it("throws when no adapter is found", async () => {
      Object.defineProperty(global, "navigator", {
        value: {
          gpu: {
            requestAdapter: jest.fn().mockResolvedValue(null),
          },
        },
        writable: true,
        configurable: true,
      });

      await expect(
        dispatchGPUCompute({
          wgslShaderUrl: "http://example.com/shader.wgsl",
          inputData: new Uint8Array([1, 2, 3]),
          outputSize: 12,
        })
      ).rejects.toThrow("No WebGPU adapter found");
    });

    it("runs full GPU pipeline with mocked device", async () => {
      const mockResultData = new Uint8Array([10, 20, 30, 40]);

      const mockStagingBuffer = {
        mapAsync: jest.fn().mockResolvedValue(undefined),
        getMappedRange: jest
          .fn()
          .mockReturnValue(mockResultData.buffer.slice(0)),
        unmap: jest.fn(),
        destroy: jest.fn(),
      };

      const mockInputBuffer = { destroy: jest.fn() };
      const mockOutputBuffer = { destroy: jest.fn() };

      const mockComputePass = {
        setPipeline: jest.fn(),
        setBindGroup: jest.fn(),
        dispatchWorkgroups: jest.fn(),
        end: jest.fn(),
      };

      const mockEncoder = {
        beginComputePass: jest.fn().mockReturnValue(mockComputePass),
        copyBufferToBuffer: jest.fn(),
        finish: jest.fn().mockReturnValue("command-buffer"),
      };

      const mockBindGroupLayout = {};
      const mockPipeline = {
        getBindGroupLayout: jest.fn().mockReturnValue(mockBindGroupLayout),
      };

      const mockShaderModule = {};
      const mockDevice = {
        createShaderModule: jest.fn().mockReturnValue(mockShaderModule),
        createComputePipeline: jest.fn().mockReturnValue(mockPipeline),
        createBuffer: jest
          .fn()
          .mockReturnValueOnce(mockInputBuffer) // input buffer
          .mockReturnValueOnce(mockOutputBuffer) // output buffer
          .mockReturnValueOnce(mockStagingBuffer), // staging buffer
        createBindGroup: jest.fn().mockReturnValue("bind-group"),
        createCommandEncoder: jest.fn().mockReturnValue(mockEncoder),
        queue: {
          writeBuffer: jest.fn(),
          submit: jest.fn(),
        },
        destroy: jest.fn(),
        lost: new Promise<void>(() => {}), // never resolves — device not lost
      };

      const mockAdapter = {
        requestDevice: jest.fn().mockResolvedValue(mockDevice),
      };

      Object.defineProperty(global, "navigator", {
        value: {
          gpu: {
            requestAdapter: jest.fn().mockResolvedValue(mockAdapter),
          },
        },
        writable: true,
        configurable: true,
      });

      // Mock GPUBufferUsage and GPUMapMode constants.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (global as any).GPUBufferUsage = {
        STORAGE: 0x80,
        COPY_DST: 0x08,
        COPY_SRC: 0x04,
        MAP_READ: 0x01,
      };
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (global as any).GPUMapMode = {
        READ: 1,
      };

      // Mock fetch for shader URL.
      const mockFetch = jest.fn().mockResolvedValue({
        ok: true,
        text: () =>
          Promise.resolve(
            "@compute @workgroup_size(256) fn main() {}"
          ),
      });
      global.fetch = mockFetch;

      const inputData = new Uint8Array([1, 2, 3]);
      const result = await dispatchGPUCompute({
        wgslShaderUrl: "http://example.com/shader.wgsl",
        inputData,
        outputSize: 4,
      });

      expect(result).toEqual(mockResultData);

      // Verify GPU pipeline was correctly orchestrated.
      expect(mockDevice.createShaderModule).toHaveBeenCalledWith({
        code: "@compute @workgroup_size(256) fn main() {}",
      });
      expect(mockDevice.createComputePipeline).toHaveBeenCalledWith({
        layout: "auto",
        compute: { module: mockShaderModule, entryPoint: "main" },
      });
      expect(mockDevice.createBuffer).toHaveBeenCalledTimes(3);
      expect(mockComputePass.setPipeline).toHaveBeenCalledWith(mockPipeline);
      expect(mockComputePass.setBindGroup).toHaveBeenCalledWith(
        0,
        "bind-group"
      );
      expect(mockComputePass.dispatchWorkgroups).toHaveBeenCalledWith(1);
      expect(mockComputePass.end).toHaveBeenCalled();
      expect(mockStagingBuffer.mapAsync).toHaveBeenCalledWith(1); // GPUMapMode.READ

      // Verify buffer cleanup (device is cached, not destroyed on success).
      expect(mockInputBuffer.destroy).toHaveBeenCalled();
      expect(mockOutputBuffer.destroy).toHaveBeenCalled();
      expect(mockStagingBuffer.destroy).toHaveBeenCalled();
      expect(mockDevice.destroy).not.toHaveBeenCalled();
    });

    it("throws when shader fetch fails", async () => {
      const mockDevice = {
        destroy: jest.fn(),
        lost: new Promise<void>(() => {}),
      };
      const mockAdapter = {
        requestDevice: jest.fn().mockResolvedValue(mockDevice),
      };

      Object.defineProperty(global, "navigator", {
        value: {
          gpu: {
            requestAdapter: jest.fn().mockResolvedValue(mockAdapter),
          },
        },
        writable: true,
        configurable: true,
      });

      global.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 404,
        statusText: "Not Found",
      });

      await expect(
        dispatchGPUCompute({
          wgslShaderUrl: "http://example.com/bad-shader.wgsl",
          inputData: new Uint8Array([1]),
          outputSize: 4,
        })
      ).rejects.toThrow("Failed to fetch WGSL shader: 404 Not Found");
    });
  });
});
