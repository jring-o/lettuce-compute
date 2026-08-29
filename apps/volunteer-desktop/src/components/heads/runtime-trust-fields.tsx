export interface RuntimeTrustChoice {
  container: boolean;
  native: boolean;
}

/** The uppercase `trusted_runtimes` list the daemon stores for a choice. */
export function trustedRuntimesFromChoice(choice: RuntimeTrustChoice): string[] {
  const out: string[] = [];
  if (choice.container) out.push("CONTAINER");
  if (choice.native) out.push("NATIVE");
  return out;
}

/** The checkbox state for a stored `trusted_runtimes` list (any case). */
export function choiceFromTrustedRuntimes(runtimes: string[]): RuntimeTrustChoice {
  const upper = runtimes.map((r) => r.toUpperCase());
  return { container: upper.includes("CONTAINER"), native: upper.includes("NATIVE") };
}

interface RuntimeTrustFieldsProps {
  headName: string;
  value: RuntimeTrustChoice;
  onChange: (next: RuntimeTrustChoice) => void;
  /** Whether this machine has a container backend the daemon can use. */
  containerAvailable: boolean;
  disabled?: boolean;
}

/**
 * The consent block for how far a head is trusted to run code on this
 * machine, shared by the Add Server dialog and the per-head trust editor. The
 * wording mirrors the CLI's attach prompt: WASM is always allowed, container
 * is offered only when a backend exists, native carries an explicit warning
 * and is never on by default.
 */
export function RuntimeTrustFields({
  headName,
  value,
  onChange,
  containerAvailable,
  disabled = false,
}: RuntimeTrustFieldsProps) {
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        A head is a trust domain: attaching to <span className="font-medium">{headName}</span> means
        trusting its operator to run code on this machine. Choose what this head may run. WASM is
        always allowed: it is fully sandboxed and cannot touch anything outside its own work folder.
      </p>

      <label className="flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          checked={value.container && containerAvailable}
          disabled={disabled || !containerAvailable}
          onChange={(e) => onChange({ ...value, container: e.target.checked })}
          aria-label="Allow container tasks from this head"
          className="mt-0.5 h-4 w-4 rounded border-input accent-primary"
        />
        <span>
          <span className="font-medium">Allow container tasks from this head</span>
          <span className="block text-xs text-muted-foreground">
            {containerAvailable
              ? "Isolated; runs through Docker or Podman."
              : "No Docker or Podman backend was detected on this machine, so container tasks cannot be offered."}
          </span>
        </span>
      </label>

      <label className="flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          checked={value.native}
          disabled={disabled}
          onChange={(e) => onChange({ ...value, native: e.target.checked })}
          aria-label="Allow native tasks from this head"
          className="mt-0.5 h-4 w-4 rounded border-input accent-primary"
        />
        <span>
          <span className="font-medium">Allow native tasks from this head</span>
          <span className="block text-xs text-amber-700 dark:text-amber-400">
            Native runs a program directly on this machine with no sandbox. It can read your files,
            including your identity key, and use your network. Enable it only for an operator you
            fully trust.
          </span>
        </span>
      </label>
    </div>
  );
}
