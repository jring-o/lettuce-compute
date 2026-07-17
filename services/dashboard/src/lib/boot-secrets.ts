/**
 * Boot-time validation of the dashboard's session secret (BG-30).
 *
 * NEXTAUTH_SECRET is the NextAuth session-JWT (JWE) key material. If it is
 * missing, left at a committed placeholder, or too short, sessions can be
 * forged, so the dashboard must refuse to start in production rather than
 * boot with a broken security posture.
 *
 * Dependency-free and pure on purpose: this module is imported by the Next.js
 * instrumentation hook (src/instrumentation.ts), which runs before the rest of
 * the app is wired up.
 *
 * The rejection rules below MUST stay identical to the head-side gate in
 * services/infrastructure/internal/config/secrets.go — the two services share
 * one operator-facing contract for what counts as a bad secret, so any change
 * here has to be mirrored there (and vice versa).
 */

// Substrings that mark a value as an obvious placeholder rather than a real
// secret. Matched case-insensitively anywhere in the value. Keep in lockstep
// with PLACEHOLDER_STEMS in internal/config/secrets.go.
const PLACEHOLDER_STEMS = [
  "change-me",
  "changeme",
  "generate-with",
  "replace-with",
  "placeholder",
  "not-for-production",
];

// Minimum acceptable secret length. Mirrors MIN_SECRET_LENGTH in
// internal/config/secrets.go. `openssl rand -base64 32` yields 44 chars, so a
// real generated secret clears this comfortably.
const MIN_SECRET_LENGTH = 32;

/**
 * Describe what is wrong with a candidate session secret, or null if it is
 * acceptable. The message is a sentence fragment meant to be prefixed with the
 * secret's name (e.g. `NEXTAUTH_SECRET ${problem}`).
 */
export function nextAuthSecretProblem(
  secret: string | undefined,
): string | null {
  const value = secret?.trim() ?? "";
  if (value.length === 0) return "missing";

  const lowered = value.toLowerCase();
  if (PLACEHOLDER_STEMS.some((stem) => lowered.includes(stem))) {
    return "is a known placeholder value";
  }

  if (value.length < MIN_SECRET_LENGTH) {
    return `is shorter than ${MIN_SECRET_LENGTH} characters`;
  }

  return null;
}

/**
 * Fail the process closed if, in production, the NextAuth session secret is
 * missing, a known placeholder, or too short.
 *
 * No-ops outside production and during the `next build` prerender pass
 * (NEXT_PHASE=phase-production-build): this is a server-START gate, and the
 * build runs with NODE_ENV=production but no real secret set.
 *
 * next-auth v5 accepts either AUTH_SECRET or NEXTAUTH_SECRET, so both are
 * considered (NEXTAUTH_SECRET wins when both are present).
 *
 * The thrown message begins with the "boot secret validation:" prefix, a
 * pinned cross-repo contract shared with the head's gate and the operator
 * guide — do not change it without updating both.
 */
export function assertProductionSecrets(
  env: Record<string, string | undefined>,
): void {
  if (env.NODE_ENV !== "production") return;
  if (env.NEXT_PHASE === "phase-production-build") return;

  const problem = nextAuthSecretProblem(env.NEXTAUTH_SECRET ?? env.AUTH_SECRET);
  if (problem === null) return;

  throw new Error(
    `boot secret validation: NEXTAUTH_SECRET ${problem}. The dashboard refuses ` +
      "to start in production with a placeholder or weak session secret. " +
      "generate: openssl rand -base64 32",
  );
}
