// WebGPU compute dispatch for browser volunteer GPU workloads.

// Cached GPU device — reused across dispatches to avoid expensive adapter/device
// negotiation on every work unit (can take 50-200ms per request).
let cachedDevice: GPUDevice | null = null;

async function getDevice(): Promise<GPUDevice> {
  if (cachedDevice) {
    // Check if device was lost (e.g., GPU reset, tab backgrounded).
    // A lost device cannot be reused — request a new one.
    try {
      // Accessing the queue on a lost device throws. A quick check.
      if (cachedDevice.lost !== undefined) {
        // device.lost is a Promise that resolves when the device is lost.
        // Check if it already resolved by racing with an immediately-resolved promise.
        const lost = await Promise.race([
          cachedDevice.lost.then(() => true),
          Promise.resolve(false),
        ]);
        if (lost) {
          cachedDevice = null;
        } else {
          return cachedDevice;
        }
      } else {
        return cachedDevice;
      }
    } catch {
      cachedDevice = null;
    }
  }

  if (typeof navigator === "undefined" || !navigator.gpu) {
    throw new Error("WebGPU is not available in this browser");
  }
  const adapter = await navigator.gpu.requestAdapter();
  if (!adapter) {
    throw new Error("No WebGPU adapter found");
  }
  const device = await adapter.requestDevice();
  device.lost.then(() => {
    if (cachedDevice === device) {
      cachedDevice = null;
    }
  });
  cachedDevice = device;
  return device;
}

/** Reset device cache — exposed for tests only. */
export function _resetDeviceCache(): void {
  cachedDevice = null;
}

export async function isWebGPUAvailable(): Promise<boolean> {
  if (typeof navigator === "undefined" || !navigator.gpu) return false;
  try {
    const adapter = await navigator.gpu.requestAdapter();
    return adapter !== null;
  } catch {
    return false;
  }
}

export async function dispatchGPUCompute(options: {
  wgslShaderUrl: string;
  inputData: Uint8Array;
  outputSize: number;
}): Promise<Uint8Array> {
  const { wgslShaderUrl, inputData, outputSize } = options;

  const device = await getDevice();

  try {
    // Fetch and compile shader.
    const shaderResp = await fetch(wgslShaderUrl);
    if (!shaderResp.ok) {
      throw new Error(
        `Failed to fetch WGSL shader: ${shaderResp.status} ${shaderResp.statusText}`
      );
    }
    const wgslCode = await shaderResp.text();
    const shaderModule = device.createShaderModule({ code: wgslCode });

    // Create pipeline.
    const pipeline = device.createComputePipeline({
      layout: "auto",
      compute: { module: shaderModule, entryPoint: "main" },
    });

    // Create buffers.
    const inputBuffer = device.createBuffer({
      size: Math.max(inputData.byteLength, 4),
      usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_DST,
    });
    device.queue.writeBuffer(inputBuffer, 0, inputData as unknown as BufferSource);

    const outputBuffer = device.createBuffer({
      size: Math.max(outputSize, 4),
      usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_SRC,
    });

    const stagingBuffer = device.createBuffer({
      size: Math.max(outputSize, 4),
      usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST,
    });

    // Create bind group.
    const bindGroup = device.createBindGroup({
      layout: pipeline.getBindGroupLayout(0),
      entries: [
        { binding: 0, resource: { buffer: inputBuffer } },
        { binding: 1, resource: { buffer: outputBuffer } },
      ],
    });

    // Dispatch compute.
    const workgroupCount = Math.max(
      1,
      Math.ceil(outputSize / 256)
    );
    const encoder = device.createCommandEncoder();
    const pass = encoder.beginComputePass();
    pass.setPipeline(pipeline);
    pass.setBindGroup(0, bindGroup);
    pass.dispatchWorkgroups(workgroupCount);
    pass.end();

    // Copy output to staging for readback.
    encoder.copyBufferToBuffer(outputBuffer, 0, stagingBuffer, 0, outputSize);
    device.queue.submit([encoder.finish()]);

    // Read back results.
    await stagingBuffer.mapAsync(GPUMapMode.READ);
    const resultData = new Uint8Array(
      stagingBuffer.getMappedRange().slice(0)
    );
    stagingBuffer.unmap();

    // Cleanup buffers.
    inputBuffer.destroy();
    outputBuffer.destroy();
    stagingBuffer.destroy();

    return resultData;
  } catch (err) {
    // On error, destroy and invalidate the cached device so the next dispatch
    // starts fresh. GPU errors (e.g., validation failures, OOM) can leave the
    // device in a bad state.
    try { device.destroy(); } catch { /* ignore */ }
    cachedDevice = null;
    throw err;
  }
}
