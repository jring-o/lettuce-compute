import type { LeafInfo, MachineCapabilities } from "@/api/client";
import { formatSizeMb, formatSizePairMb } from "@/lib/utils";
import { memoryStopAtOrAboveMb } from "@/lib/resource-limits";

/** A runtime a leaf's execution spec can be run under. */
export type LeafRuntime = "container" | "native" | "wasm";

/**
 * Which runtimes a leaf's execution spec offers: an image means container,
 * a `wasm` binary means WASM, any other binary key means native.
 */
/** Binary-map keys that never denote a native executable. */
const NON_NATIVE_BINARY_KEYS = new Set(["wasm", "wgsl", "viz"]);

export function leafRuntimes(leaf: LeafInfo): LeafRuntime[] {
  const spec = leaf.execution_spec;
  const out: LeafRuntime[] = [];
  if (spec?.image) out.push("container");
  const keys = Object.keys(spec?.binaries ?? {});
  if (keys.some((k) => !NON_NATIVE_BINARY_KEYS.has(k))) out.push("native");
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

/**
 * One item of the "Needs:" line. `shortfall` is set when this machine falls
 * short; the label and the shortfall then print their figures in the same
 * units and never round to the same number (TB-66).
 */
export interface RequirementItem {
  /** "disk", "memory", "cores" or "gpu". */
  key: string;
  label: string;
  shortfall?: string;
  /**
   * Memory only, with `shortfall`: the first Memory-slider stop that clears
   * it, in MB — the value to raise `max_memory_mb` to. Absent when the
   * container engine's virtual machine is the bound (`vmLimited`): no slider
   * stop clears a shortfall the machine's size causes.
   */
  raiseToMb?: number;
  /**
   * Memory only, with `shortfall`: the container engine's virtual machine,
   * not the Settings allowance, is what falls short (TB-63). The remedy is to
   * enlarge the machine, so the card offers no allowance raise.
   */
  vmLimited?: boolean;
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
 * The budget a leaf is compared with: container work's for a container leaf
 * (one whose spec names an image — the runtime the daemon picks first), the
 * Settings allowance for any other, which runs on the machine itself rather
 * than inside the container engine's virtual machine (TB-85). A host figure
 * the daemon did not report falls back to container work's.
 */
function budgetFor(container: boolean, containerBudget: number, hostBudget: number): number {
  return container || !(hostBudget > 0) ? containerBudget : hostBudget;
}

/**
 * The machine budgets a leaf needs, compared against what the running daemon
 * advertises for the leaf's runtime (TB-85). The comparison mirrors the CLI's
 * `doctor` (classifyLeaf): a budget the daemon reports as 0 is unknown, not
 * zero, and is skipped; the
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
  const container = !!spec?.image;

  const minDisk = rr?.min_disk_mb ?? 0;
  if (minDisk > 0) {
    const item: RequirementItem = { key: "disk", label: `${formatSizeMb(minDisk)} disk` };
    if (machine && machine.max_disk_mb > 0 && minDisk > machine.max_disk_mb) {
      const [need, have] = formatSizePairMb(minDisk, machine.max_disk_mb);
      item.label = `${need} disk`;
      item.shortfall = `you allow ${have}`;
    }
    items.push(item);
  }

  const memory = spec?.max_memory_mb ?? 0;
  if (memory > 0) {
    const item: RequirementItem = { key: "memory", label: `${formatSizeMb(memory)} RAM` };
    const budget = machine ? budgetFor(container, machine.max_memory_mb, machine.host_max_memory_mb) : 0;
    if (machine && budget > 0 && memory > budget) {
      const [need, have] = formatSizePairMb(memory, budget);
      item.label = `${need} RAM`;
      if (container && machine.memory_limited_by_vm) {
        // The budget is what the container engine's virtual machine can
        // hold, not what Settings allows (TB-63): name the machine and its
        // size — in the same unit as the pair, so the three figures read
        // together — and offer no slider stop, since none would clear it.
        const vm = have.endsWith(" MiB")
          ? `${machine.container_vm_memory_mb} MiB`
          : formatSizeMb(machine.container_vm_memory_mb);
        item.shortfall = `the container engine's virtual machine allows ${have}; it has ${vm}`;
        item.vmLimited = true;
      } else {
        item.shortfall = `you allow ${have}`;
        item.raiseToMb = memoryStopAtOrAboveMb(memory);
      }
    }
    items.push(item);
  }

  const cores = rr?.min_cpu_cores ?? 0;
  if (cores > 0) {
    const item: RequirementItem = { key: "cores", label: `${cores} ${cores === 1 ? "core" : "cores"}` };
    const budget = machine ? budgetFor(container, machine.max_cpu_cores, machine.host_max_cpu_cores) : 0;
    if (machine && budget > 0 && cores > budget) {
      if (container && machine.cpu_limited_by_vm) {
        // The budget is the container engine's virtual machine CPU count,
        // not what Settings allows (TB-75): name the machine, as for memory.
        item.shortfall = `the container engine's virtual machine allows ${budget}; it has ${machine.container_vm_cpus} CPUs`;
        item.vmLimited = true;
      } else {
        item.shortfall = `you allow ${budget}`;
      }
    }
    items.push(item);
  }

  const specGpu = !!spec?.gpu_required;
  const rrGpu = !!rr?.gpu_required;
  if (specGpu || rrGpu) {
    const vendor = specificGpuType(rr?.gpu_type ?? spec?.gpu_type);
    const vram = rr?.min_gpu_vram_mb ?? 0;
    const capability = (rr?.gpu_compute_capability ?? "").trim();

    // The VRAM figure is settled with its shortfall so the two print in the
    // same units (formatSizePairMb).
    let vramLabel = vram > 0 ? formatSizeMb(vram) : "";
    let shortfall: string | undefined;
    if (machine) {
      if (!machine.has_gpu) {
        shortfall = "no GPU detected or enabled";
      } else if (
        specGpu &&
        vendor &&
        machine.gpu_vendors.length > 0 &&
        !containsFold(machine.gpu_vendors, vendor)
      ) {
        shortfall = `yours is ${machine.gpu_vendors.join("/")}`;
      } else if (
        rrGpu &&
        capability &&
        machine.gpu_compute_capabilities.length > 0 &&
        !containsFold(machine.gpu_compute_capabilities, capability)
      ) {
        shortfall = `yours is ${machine.gpu_compute_capabilities.join("/")}`;
      } else if (vram > 0 && machine.gpu_card_vram_mb > 0 && vram > machine.gpu_card_vram_mb) {
        const [need, card] = formatSizePairMb(vram, machine.gpu_card_vram_mb);
        vramLabel = need;
        shortfall = `your ${card} card is too small whatever percentage you allow`;
      } else if (vram > 0 && machine.max_gpu_vram_mb > 0 && vram > machine.max_gpu_vram_mb) {
        const [need, have] = formatSizePairMb(vram, machine.max_gpu_vram_mb);
        vramLabel = need;
        shortfall =
          machine.gpu_card_vram_mb > 0 && machine.gpu_vram_pct > 0
            ? `your allowance is ${have} (${machine.gpu_vram_pct}% of a ${formatSizeMb(machine.gpu_card_vram_mb)} card)`
            : `your allowance is ${have}`;
      }
    }

    const parts = [vendor ? `${vendor} GPU` : "a GPU"];
    if (vram > 0) parts.push(`${vramLabel} VRAM`);
    if (capability) parts.push(`compute capability ${capability}`);
    const item: RequirementItem = { key: "gpu", label: parts.join(", ") };
    if (shortfall) item.shortfall = shortfall;
    items.push(item);
  }

  return items;
}
