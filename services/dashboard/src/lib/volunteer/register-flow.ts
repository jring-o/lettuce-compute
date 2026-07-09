// Registration with reactive proof-of-work — the browser twin of the volunteer CLI's
// registerWithPow. The first register attempt is always bare, so nothing is paid unless
// the head demands it (and with enforcement off this collapses to the single legacy
// call). On a POW_REQUIRED refusal: fetch a challenge, solve it off-thread, retry once;
// if that solution is rejected (POW_INVALID), fetch a FRESH challenge, re-solve, retry
// once more, then surface whatever the head returns. Bounded by construction — never a
// loop.

import { ApiError, type VolunteerClient } from "./client";
import { solveInWorker, PowSolveCancelledError } from "./pow-solver";
import type {
  HardwareCapabilities,
  RegisterChallengeResponse,
  RegisterPow,
  RegisterResponse,
} from "./types";

export interface RegisterFlowHooks {
  // Fired when a solve is about to start (the challenge is in hand, so the difficulty
  // is known). Fires again for the POW_INVALID re-solve.
  onSolvingStart?: (difficultyBits: number) => void;
  // Forwarded from the solver's attempt-count cadence.
  onSolveProgress?: (attempts: number) => void;
  // Fired after each successful solve.
  onSolved?: (attempts: number, elapsedMs: number) => void;
  // Test seams. Production callers leave these unset.
  solve?: typeof solveInWorker;
  now?: () => number;
}

export interface RegisterFlowHandle {
  promise: Promise<RegisterResponse>;
  // Cancels a solve in flight (worker terminated) and aborts the flow at the next
  // phase boundary; the promise rejects with PowSolveCancelledError.
  cancel: () => void;
}

function isCode(err: unknown, status: number, code: string): boolean {
  return err instanceof ApiError && err.status === status && err.code === code;
}

export function registerWithPow(
  client: VolunteerClient,
  hardware: HardwareCapabilities,
  // The registering key, base64url — the head binds the challenge to this key and the
  // solution digest binds it again, so it must be the same identity the client signs
  // and registers with.
  publicKeyBase64url: string,
  hooks: RegisterFlowHooks = {}
): RegisterFlowHandle {
  const solve = hooks.solve ?? solveInWorker;
  const now = hooks.now ?? Date.now;

  let cancelled = false;
  let activeCancel: (() => void) | null = null;

  function assertNotCancelled(): void {
    if (cancelled) throw new PowSolveCancelledError();
  }

  async function solveChallenge(ch: RegisterChallengeResponse): Promise<string> {
    hooks.onSolvingStart?.(ch.difficulty_bits);
    const started = now();
    const handle = solve(
      ch.challenge_hex,
      publicKeyBase64url,
      ch.difficulty_bits,
      hooks.onSolveProgress
    );
    activeCancel = handle.cancel;
    try {
      const solution = await handle.promise;
      hooks.onSolved?.(solution.attempts, now() - started);
      return solution.nonce;
    } finally {
      activeCancel = null;
    }
  }

  // Fetch a challenge and solve it. The TTL pre-submit guard re-fetches AT MOST once
  // and then submits regardless (the CLI twin's exact bound): under client clock skew
  // every challenge "looks expired", and submitting anyway lets the head adjudicate —
  // a genuinely stale solution lands in the caller's POW_INVALID retry.
  async function fetchAndSolve(): Promise<RegisterPow> {
    let ch = await client.fetchRegistrationChallenge();
    assertNotCancelled();
    let nonce = await solveChallenge(ch);
    const expiry = Date.parse(ch.expires_at);
    if (Number.isFinite(expiry) && now() >= expiry) {
      assertNotCancelled();
      ch = await client.fetchRegistrationChallenge();
      assertNotCancelled();
      nonce = await solveChallenge(ch);
    }
    return { challengeId: ch.challenge_id, nonce };
  }

  const promise = (async (): Promise<RegisterResponse> => {
    try {
      return await client.register(hardware);
    } catch (err) {
      if (!isCode(err, 403, "POW_REQUIRED")) throw err;
    }
    assertNotCancelled();

    // pow-required: solve a fresh challenge and retry once.
    try {
      return await client.register(hardware, await fetchAndSolve());
    } catch (err) {
      if (!isCode(err, 400, "POW_INVALID")) throw err;
    }
    assertNotCancelled();

    // The first solution was rejected: fresh challenge, re-solve, retry once more,
    // then surface whatever the head returns.
    return client.register(hardware, await fetchAndSolve());
  })();

  return {
    promise,
    cancel: () => {
      cancelled = true;
      activeCancel?.();
    },
  };
}
