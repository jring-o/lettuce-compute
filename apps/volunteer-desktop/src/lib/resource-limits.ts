/**
 * The shape of the Memory slider on the Settings page, shared with the leaf
 * card that names the stop a leaf needs, so the two can never disagree about
 * what the slider can be set to (TB-66).
 */
export const MEMORY_SLIDER_STEP_MB = 256;

/** The most the Memory slider offers on its own: 90 % of the machine's RAM, in MB. */
export function memoryAllowanceCeilingMb(totalMemMb: number): number {
  return Math.round(totalMemMb * 0.9);
}

/**
 * The slider's maximum. The ceiling is never a clamp: an allowance already
 * saved above it (a raise from a leaf card, an older config) is shown as it
 * is rather than truncated the first time the slider is touched.
 */
export function memorySliderMaxMb(totalMemMb: number, currentMb: number): number {
  return Math.max(memoryAllowanceCeilingMb(totalMemMb), currentMb);
}

/**
 * The first Memory-slider stop at or above `mb`: the value `max_memory_mb`
 * must be raised to for a leaf declaring `mb` to fit. The slider moves in
 * 256 MB steps, so a requirement such as 7000 MB is not on a stop — the stop
 * just below it (6912 MB) still fails the head's check, and the one to set is
 * the next one up (7168 MB).
 */
export function memoryStopAtOrAboveMb(mb: number): number {
  return Math.max(
    MEMORY_SLIDER_STEP_MB,
    Math.ceil(mb / MEMORY_SLIDER_STEP_MB) * MEMORY_SLIDER_STEP_MB
  );
}
