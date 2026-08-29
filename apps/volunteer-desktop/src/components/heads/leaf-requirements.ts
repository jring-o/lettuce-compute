import type { LeafInfo, MachineCapabilities } from "@/api/client";
import { formatSizeMb } from "@/lib/utils";

/** A runtime a leaf's execution spec can be run under. */
export type LeafRuntime = "container" | "native" | "wasm";

/**
 * Which runtimes a leaf's execution spec offers: an image means container,
 * a `wasm` binary means WASM, any other binary key means native.
 */
export function leafRuntimes(leaf: LeafInfo): LeafRuntime[] {
  const spec = leaf.execution_spec;
  const out: LeafRuntime[] = [];
  if (spec?.image) out.push("container");
  const keys = Object.keys(spec?.binaries ?? {});
  if (keys.some((k) => k !== "wasm" && k !== "wgsl")) out.push("native");
  if (spec?.binaries?.wasm) out.push("wasm");
  return out;
}

/**
 * Whether this head is trusted to run `runtime` here. WASM is always
 * trusted; `null` trust (not loaded) is treated as trusted so nothing is
 * greyed on a guess.
 */
export function runtimeTrusted(runtime: LeafRuntime, trustedRuntimes: string[] | null): boolean {
  if (runtime === "wasm" || trustedRuntimes === null) return true;
  return trustedRuntimes.some((r) => r.toUpperCase() === runtime.toUpperCase());
}

/** One item of the "Needs:" line. `shortfall` is set when this machine falls short. */
export interface RequirementItem {
  /** "disk", "memory", "cores" or "gpu". */
  key: string;
  label: string;
  shortfall?: string;
}

function specificGpuType(gpuType: string | undefined): string | null {
  const t = (gpuType ?? "").trim().toUpperCase();
  return t && t !== "ANY" ? t : null;
}

function containsFold(list: string[], value: string): boolean {
  const v = value.toUpperCase();
  return list.some((item) => item.toUpperCase() === v);
}

/**
 * The machine budgets a leaf needs, compared against what the running daemon
 * advertises. The comparison mirrors the CLI's `doctor` (classifyLeaf): a
 * budget the daemon reports as 0 is unknown, not zero, and is skipped; the
 * vendor gate keys on the execution spec's `gpu_required`, the compute
 * capability gate on the requirements' own flag, because that is how the
 * head's dispatch predicate keys them. VRAM is compared against the ALLOWED
 * figure (card size x the VRAM percentage), never the card size. With no
 * `machine` (not loaded yet) nothing is marked short.
 */
export function leafRequirementItems(
  leaf: LeafInfo,
  machine: MachineCapabilities | null
): RequirementItem[] {
  const spec = leaf.execution_spec;
  const rr = leaf.resource_requirements;
  const items: RequirementItem[] = [];

  const minDisk = rr?.min_disk_mb ?? 0;
  if (minDisk > 0) {
    const item: RequirementItem = { key: "disk", label: `${formatSizeMb(minDisk)} disk` };
    if (machine && machine.max_disk_mb > 0 && minDisk > machine.max_disk_mb) {
      item.shortfall = `you allow ${formatSizeMb(machine.max_disk_mb)}`;
    }
    items.push(item);
  }

  const memory = spec?.max_memory_mb ?? 0;
  if (memory > 0) {
    const item: RequirementItem = { key: "memory", label: `${formatSizeMb(memory)} RAM` };
    if (machine && machine.max_memory_mb > 0 && memory > machine.max_memory_mb) {
      item.shortfall = `you allow ${formatSizeMb(machine.max_memory_mb)}`;
    }
    items.push(item);
  }

  const cores = rr?.min_cpu_cores ?? 0;
  if (cores > 0) {
    const item: RequirementItem = { key: "cores", label: `${cores} ${cores === 1 ? "core" : "cores"}` };
    if (machine && machine.max_cpu_cores > 0 && cores > machine.max_cpu_cores) {
      item.shortfall = `you allow ${machine.max_cpu_cores}`;
    }
    items.push(item);
  }

  const specGpu = !!spec?.gpu_required;
  const rrGpu = !!rr?.gpu_required;
  if (specGpu || rrGpu) {
    const vendor = specificGpuType(rr?.gpu_type ?? spec?.gpu_type);
    const vram = rr?.min_gpu_vram_mb ?? 0;
    const capability = (rr?.gpu_compute_capability ?? "").trim();

    const parts = [vendor ? `${vendor} GPU` : "a GPU"];
    if (vram > 0) parts.push(`${formatSizeMb(vram)} VRAM`);
    if (capability) parts.push(`compute capability ${capability}`);
    const item: RequirementItem = { key: "gpu", label: parts.join(", ") };

    if (machine) {
      if (!machine.has_gpu) {
        item.shortfall = "no GPU detected or enabled";
      } else if (
        specGpu &&
        vendor &&
        machine.gpu_vendors.length > 0 &&
        !containsFold(machine.gpu_vendors, vendor)
      ) {
        item.shortfall = `yours is ${machine.gpu_vendors.join("/")}`;
      } else if (
        rrGpu &&
        capability &&
        machine.gpu_compute_capabilities.length > 0 &&
        !containsFold(machine.gpu_compute_capabilities, capability)
      ) {
        item.shortfall = `yours is ${machine.gpu_compute_capabilities.join("/")}`;
      } else if (vram > 0 && machine.gpu_card_vram_mb > 0 && vram > machine.gpu_card_vram_mb) {
        item.shortfall = `your ${formatSizeMb(machine.gpu_card_vram_mb)} card is too small whatever percentage you allow`;
      } else if (vram > 0 && machine.max_gpu_vram_mb > 0 && vram > machine.max_gpu_vram_mb) {
        item.shortfall =
          machine.gpu_card_vram_mb > 0 && machine.gpu_vram_pct > 0
            ? `your allowance is ${formatSizeMb(machine.max_gpu_vram_mb)} (${machine.gpu_vram_pct}% of a ${formatSizeMb(machine.gpu_card_vram_mb)} card)`
            : `your allowance is ${formatSizeMb(machine.max_gpu_vram_mb)}`;
      }
    }
    items.push(item);
  }

  return items;
}
