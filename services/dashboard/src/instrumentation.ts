import { assertProductionSecrets } from "@/lib/boot-secrets";

/**
 * Next.js auto-detects this instrumentation hook (it lives in src/ and runs
 * once as the server starts). It fails the dashboard server closed at start
 * under NODE_ENV=production when the NextAuth session secret is missing, a
 * known placeholder, or shorter than 32 characters (BG-30). Development and the
 * `next build` prerender pass are unaffected.
 */
export async function register() {
  assertProductionSecrets(process.env);
}
