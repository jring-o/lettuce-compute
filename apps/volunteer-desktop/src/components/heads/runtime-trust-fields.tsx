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

/**
 * The note shown under the container option when this machine has no
 * container engine the daemon can use.
 */
export const CONTAINER_UNAVAILABLE_NOTE =
  "No Docker or Podman backend was detected on this machine, so container tasks cannot be offered.";

interface RuntimeTrustFieldsProps {
  headName: string;
  value: RuntimeTrustChoice;
  onChange: (next: RuntimeTrustChoice) => void;
  /** Whether this machine has a container backend the daemon can use. */
  containerAvailable: boolean;
  /**
   * Why container tasks cannot be offered, shown under the container option
   * while `containerAvailable` is false. Defaults to `CONTAINER_UNAVAILABLE_NOTE`;
   * a caller that knows more (the setup wizard, which just probed the machine)
   * can say so.
   */
  containerUnavailableNote?: string;
  disabled?: boolean;
}

/**
 * The consent block for how far a head is trusted to run code on this
 * machine. It is the one place this question is asked: the setup wizard's
 * Connect step, the Add Server dialog and the per-head trust editor all
 * render it. The wording mirrors the CLI's attach prompt: WASM is always
 * allowed, container is offered only when a backend exists (and is the
 * caller's default when it does), native carries an explicit warning and is
 * never on by default.
 */
export function RuntimeTrustFields({
  headName,
  value,
  onChange,
  containerAvailable,
  containerUnavailableNote = CONTAINER_UNAVAILABLE_NOTE,
  disabled = false,
}: RuntimeTrustFieldsProps) {
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        A head is a trust domain: attaching to <span className="font-medium">{headName}</span> means
        trusting its operator to run code on this machine. Choose what this head may run. You can
        change this later.
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
              : containerUnavailableNote}
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
            including your identity key, and use your network. Allow this only for an operator you
            fully trust.
          </span>
        </span>
      </label>

      <p className="text-xs text-muted-foreground">
        WASM tasks are always allowed (sandboxed): they cannot touch anything outside their own work
        folder.
      </p>
    </div>
  );
}
