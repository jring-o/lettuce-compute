import { describe, it, expect } from "vitest";
import type { LeafInfo, MachineCapabilities } from "@/api/client";
import { leafRuntimes, runtimeTrusted, leafRequirementItems } from "./leaf-requirements";

function makeLeaf(overrides: Partial<LeafInfo> = {}): LeafInfo {
  return {
    id: "leaf-1",
    slug: "prime",
    name: "Prime Study",
    research_area: ["mathematics"],
    task_pattern: "PARAMETER_SWEEP",
    state: "ACTIVE",
    queued_work_units: 10,
    active_volunteers: 1,
    active_hosts: 1,
    enabled: true,
    effective_weight: 100,
    ...overrides,
  };
}

function makeMachine(overrides: Partial<MachineCapabilities> = {}): MachineCapabilities {
  return {
    runtimes: ["container", "wasm"],
    has_gpu: true,
    max_memory_mb: 8192,
    container_vm_memory_mb: 0,
    memory_limited_by_vm: false,
    max_disk_mb: 10240,
    max_cpu_cores: 4,
    max_gpu_vram_mb: 2048,
    gpu_card_vram_mb: 4096,
    gpu_vram_pct: 50,
    gpu_vendors: ["NVIDIA"],
    gpu_compute_capabilities: ["8.6"],
    ...overrides,
  };
}

describe("leafRuntimes", () => {
  it("reads container, native and wasm from the execution spec", () => {
    expect(leafRuntimes(makeLeaf())).toEqual([]);
    expect(leafRuntimes(makeLeaf({ execution_spec: { image: "ghcr.io/x:1" } }))).toEqual(["container"]);
    expect(
      leafRuntimes(makeLeaf({ execution_spec: { binaries: { "linux-amd64": "/bin", wasm: "m.wasm" } } }))
    ).toEqual(["native", "wasm"]);
  });

  it("does not count a visualization bundle as a native binary", () => {
    expect(
      leafRuntimes({ execution_spec: { image: "lbry.science/beyblade:2.1", binaries: { viz: "https://x/viz.tar.gz" } } } as never),
    ).toEqual(["container"]);
  });

  it("does not count a wgsl shader as a native binary", () => {
    expect(leafRuntimes(makeLeaf({ execution_spec: { binaries: { wgsl: "k.wgsl", wasm: "m.wasm" } } }))).toEqual(["wasm"]);
  });
});

describe("runtimeTrusted", () => {
  it("always trusts wasm and treats unknown trust as trusted", () => {
    expect(runtimeTrusted("wasm", [])).toBe(true);
    expect(runtimeTrusted("native", null)).toBe(true);
  });

  it("matches trusted runtimes case-insensitively", () => {
    expect(runtimeTrusted("container", ["CONTAINER"])).toBe(true);
    expect(runtimeTrusted("container", ["container"])).toBe(true);
    expect(runtimeTrusted("native", ["CONTAINER"])).toBe(false);
  });
});

describe("leafRequirementItems", () => {
  it("returns nothing for a leaf without requirements", () => {
    expect(leafRequirementItems(makeLeaf(), makeMachine())).toEqual([]);
  });

  it("lists disk, RAM, cores and GPU in order with no shortfall on a capable machine", () => {
    const leaf = makeLeaf({
      execution_spec: { max_memory_mb: 7168, gpu_required: true },
      resource_requirements: { min_disk_mb: 15360, min_cpu_cores: 1, min_gpu_vram_mb: 1024, gpu_type: "NVIDIA" },
    });
    const items = leafRequirementItems(leaf, makeMachine({ max_disk_mb: 20480 }));
    expect(items.map((i) => i.label)).toEqual(["15 GB disk", "7 GB RAM", "1 core", "NVIDIA GPU, 1 GB VRAM"]);
    expect(items.every((i) => i.shortfall === undefined)).toBe(true);
  });

  it("marks disk, RAM and cores the machine falls short on", () => {
    const leaf = makeLeaf({
      execution_spec: { max_memory_mb: 16384 },
      resource_requirements: { min_disk_mb: 15360, min_cpu_cores: 8 },
    });
    const items = leafRequirementItems(leaf, makeMachine());
    expect(items).toEqual([
      { key: "disk", label: "15 GB disk", shortfall: "you allow 10 GB" },
      { key: "memory", label: "16 GB RAM", shortfall: "you allow 8 GB", raiseToMb: 16384 },
      { key: "cores", label: "8 cores", shortfall: "you allow 4" },
    ]);
  });

  it("treats a budget the daemon reports as 0 as unknown, not as a shortfall", () => {
    const leaf = makeLeaf({ resource_requirements: { min_disk_mb: 15360, min_cpu_cores: 8 } });
    const items = leafRequirementItems(leaf, makeMachine({ max_disk_mb: 0, max_cpu_cores: 0 }));
    expect(items.every((i) => i.shortfall === undefined)).toBe(true);
  });

  it("marks nothing when the machine is not loaded yet", () => {
    const leaf = makeLeaf({ resource_requirements: { min_disk_mb: 999999 } });
    expect(leafRequirementItems(leaf, null)[0].shortfall).toBeUndefined();
  });

  it("compares VRAM against the allowance and explains where it comes from", () => {
    const leaf = makeLeaf({
      execution_spec: { gpu_required: true },
      resource_requirements: { min_gpu_vram_mb: 3072 },
    });
    const [gpu] = leafRequirementItems(leaf, makeMachine());
    expect(gpu.label).toBe("a GPU, 3 GB VRAM");
    expect(gpu.shortfall).toBe("your allowance is 2 GB (50% of a 4 GB card)");
  });

  it("says when the card itself is too small", () => {
    const leaf = makeLeaf({
      execution_spec: { gpu_required: true },
      resource_requirements: { min_gpu_vram_mb: 8192 },
    });
    const [gpu] = leafRequirementItems(leaf, makeMachine());
    expect(gpu.shortfall).toBe("your 4 GB card is too small whatever percentage you allow");
  });

  it("reports a missing GPU before anything else", () => {
    const leaf = makeLeaf({
      resource_requirements: { gpu_required: true, min_gpu_vram_mb: 1 },
    });
    const [gpu] = leafRequirementItems(leaf, makeMachine({ has_gpu: false }));
    expect(gpu.shortfall).toBe("no GPU detected or enabled");
  });

  it("gates the vendor on the execution spec flag and ignores ANY", () => {
    const amd = makeMachine({ gpu_vendors: ["AMD"] });
    const specFlag = makeLeaf({
      execution_spec: { gpu_required: true },
      resource_requirements: { gpu_type: "NVIDIA" },
    });
    expect(leafRequirementItems(specFlag, amd)[0].shortfall).toBe("yours is AMD");

    const rrFlagOnly = makeLeaf({ resource_requirements: { gpu_required: true, gpu_type: "NVIDIA" } });
    expect(leafRequirementItems(rrFlagOnly, amd)[0].shortfall).toBeUndefined();

    const any = makeLeaf({ execution_spec: { gpu_required: true }, resource_requirements: { gpu_type: "ANY" } });
    expect(leafRequirementItems(any, amd)[0]).toEqual({ key: "gpu", label: "a GPU" });
  });

  it("gates compute capability on the requirements flag", () => {
    const machine = makeMachine({ gpu_compute_capabilities: ["7.5"] });
    const leaf = makeLeaf({
      resource_requirements: { gpu_required: true, gpu_compute_capability: "8.6" },
    });
    const [gpu] = leafRequirementItems(leaf, machine);
    expect(gpu.label).toBe("a GPU, compute capability 8.6");
    expect(gpu.shortfall).toBe("yours is 7.5");

    const specOnly = makeLeaf({
      execution_spec: { gpu_required: true },
      resource_requirements: { gpu_compute_capability: "8.6" },
    });
    expect(leafRequirementItems(specOnly, machine)[0].shortfall).toBeUndefined();
  });
});

// TB-66: the tester's Mac mini — three GREP leaves declaring 7000 MB, the
// Memory slider set to the stop that read the same "6.8 GB" as the card
// (6912 MB), and the head refusing every poll on `7000 <= 6912`.
describe("TB-66: a shortfall's two figures never round to the same label", () => {
  it("prints a 7000 MB requirement and a 6912 MB allowance in MB, and names the stop that clears it", () => {
    const leaf = makeLeaf({ execution_spec: { max_memory_mb: 7000 } });
    const [memory] = leafRequirementItems(leaf, makeMachine({ max_memory_mb: 6912 }));
    expect(memory).toEqual({
      key: "memory",
      label: "7000 MB RAM",
      shortfall: "you allow 6912 MB",
      raiseToMb: 7168,
    });
  });

  it("keeps whole-gigabyte figures short and still names the stop", () => {
    const leaf = makeLeaf({ execution_spec: { max_memory_mb: 16384 } });
    const [memory] = leafRequirementItems(leaf, makeMachine({ max_memory_mb: 8192 }));
    expect(memory).toEqual({
      key: "memory",
      label: "16 GB RAM",
      shortfall: "you allow 8 GB",
      raiseToMb: 16384,
    });
  });

  it("names no stop when the machine is not short", () => {
    const leaf = makeLeaf({ execution_spec: { max_memory_mb: 7000 } });
    const [memory] = leafRequirementItems(leaf, makeMachine({ max_memory_mb: 7168 }));
    expect(memory).toEqual({ key: "memory", label: "6.8 GB RAM" });
  });

  it("applies the same rule to disk and VRAM", () => {
    const leaf = makeLeaf({
      execution_spec: { gpu_required: true },
      resource_requirements: { min_disk_mb: 15000, min_gpu_vram_mb: 2100 },
    });
    const [disk, gpu] = leafRequirementItems(
      leaf,
      makeMachine({ max_disk_mb: 14336, max_gpu_vram_mb: 2048, gpu_card_vram_mb: 4096, gpu_vram_pct: 50 })
    );
    expect(disk).toEqual({ key: "disk", label: "15000 MB disk", shortfall: "you allow 14336 MB" });
    expect(gpu).toEqual({
      key: "gpu",
      label: "a GPU, 2100 MB VRAM",
      shortfall: "your allowance is 2048 MB (50% of a 4 GB card)",
    });

    const tooSmallCard = leafRequirementItems(
      makeLeaf({ execution_spec: { gpu_required: true }, resource_requirements: { min_gpu_vram_mb: 4200 } }),
      makeMachine({ gpu_card_vram_mb: 4096 })
    )[0];
    expect(tooSmallCard.label).toBe("a GPU, 4200 MB VRAM");
    expect(tooSmallCard.shortfall).toBe("your 4096 MB card is too small whatever percentage you allow");
  });
});

describe("TB-63: the container engine's virtual machine bounds memory", () => {
  // The tester's Mac: 8192 MB allowed in Settings, a 2 GiB Podman machine, so
  // 1536 MB for container work. The card used to say "you allow 8 GB" beside a
  // 7000 MB leaf the machine killed at model load, and TB-66's raise button
  // would have offered a slider stop that changes nothing.
  const vmMachine = () =>
    makeMachine({ max_memory_mb: 1536, container_vm_memory_mb: 2048, memory_limited_by_vm: true });

  it("names the machine and its size instead of the allowance, and offers no slider stop", () => {
    const leaf = makeLeaf({ execution_spec: { max_memory_mb: 7000 } });
    const memory = leafRequirementItems(leaf, vmMachine()).find((i) => i.key === "memory");
    expect(memory).toEqual({
      key: "memory",
      label: "7000 MB RAM",
      shortfall: "the container engine's virtual machine allows 1536 MB; it has 2048 MB",
      vmLimited: true,
    });
    expect(memory?.raiseToMb).toBeUndefined();
  });

  it("prints all three figures in GB when none would round", () => {
    const machine = makeMachine({
      max_memory_mb: 3072,
      container_vm_memory_mb: 4096,
      memory_limited_by_vm: true,
    });
    const leaf = makeLeaf({ execution_spec: { max_memory_mb: 8192 } });
    const memory = leafRequirementItems(leaf, machine).find((i) => i.key === "memory");
    expect(memory?.label).toBe("8 GB RAM");
    expect(memory?.shortfall).toBe("the container engine's virtual machine allows 3 GB; it has 4 GB");
  });

  it("marks nothing when the leaf fits the machine's budget", () => {
    const leaf = makeLeaf({ execution_spec: { max_memory_mb: 1024 } });
    const memory = leafRequirementItems(leaf, vmMachine()).find((i) => i.key === "memory");
    expect(memory).toEqual({ key: "memory", label: "1 GB RAM" });
  });

  it("keeps the allowance wording and the slider stop when the machine is not the bound", () => {
    const machine = makeMachine({ max_memory_mb: 6912, container_vm_memory_mb: 8192 });
    const leaf = makeLeaf({ execution_spec: { max_memory_mb: 7000 } });
    const memory = leafRequirementItems(leaf, machine).find((i) => i.key === "memory");
    expect(memory).toEqual({
      key: "memory",
      label: "7000 MB RAM",
      shortfall: "you allow 6912 MB",
      raiseToMb: 7168,
    });
  });
});
