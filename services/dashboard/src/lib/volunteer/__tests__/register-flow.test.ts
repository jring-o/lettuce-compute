import { registerWithPow } from "../register-flow";
import { ApiError, type VolunteerClient } from "../client";
import { PowSolveCancelledError } from "../pow-solver";
import type {
  HardwareCapabilities,
  PowSolution,
  RegisterChallengeResponse,
} from "../types";

// The flow is pure orchestration over an injected client and solver, so these tests
// pin the retry-shape contract (the CLI twin's bounds) without React or real workers.

const hardware: HardwareCapabilities = {
  cpu_cores: 4,
  memory_mb: 8192,
  has_gpu: false,
  gpu_vendors: [],
  available_runtimes: ["WASM"],
};

const PUB_KEY = "mock-public-key-b64url";

const REGISTERED = { volunteer_id: "vol-1", registered_at: "2026-07-09T00:00:00Z" };

// A far-future expiry so the TTL guard stays dormant unless a test wants it.
const FUTURE = "2999-01-01T00:00:00Z";

function challenge(id: string, expiresAt: string = FUTURE): RegisterChallengeResponse {
  return {
    challenge_id: id,
    challenge_hex: "ab".repeat(32),
    difficulty_bits: 20,
    expires_at: expiresAt,
  };
}

function apiError(status: number, code: string | null): ApiError {
  return new ApiError(status, code, code, `Registration failed: ${status}`);
}

// A solver stub that resolves immediately with a fixed nonce, counting invocations.
function immediateSolver(nonce = "19497") {
  const calls: Array<{ challengeHex: string; publicKey: string; bits: number }> = [];
  const cancel = jest.fn();
  const solve = (challengeHex: string, publicKey: string, bits: number) => {
    calls.push({ challengeHex, publicKey, bits });
    return {
      promise: Promise.resolve<PowSolution>({ nonce, attempts: 19498 }),
      cancel,
    };
  };
  return { solve, calls, cancel };
}

function mockClient(overrides: Partial<VolunteerClient>): VolunteerClient {
  return {
    register: jest.fn().mockResolvedValue(REGISTERED),
    fetchRegistrationChallenge: jest.fn().mockResolvedValue(challenge("chal-1")),
    requestWork: jest.fn(),
    submitResult: jest.fn(),
    ...overrides,
  };
}

describe("registerWithPow", () => {
  it("resolves on a bare register without ever fetching a challenge", async () => {
    const { solve, calls } = immediateSolver();
    const client = mockClient({});

    const result = await registerWithPow(client, hardware, PUB_KEY, { solve }).promise;

    expect(result).toEqual(REGISTERED);
    expect(client.register).toHaveBeenCalledTimes(1);
    expect(client.register).toHaveBeenCalledWith(hardware);
    expect(client.fetchRegistrationChallenge).not.toHaveBeenCalled();
    expect(calls).toHaveLength(0);
  });

  it("solves and retries once on POW_REQUIRED", async () => {
    const { solve, calls } = immediateSolver("42");
    const register = jest
      .fn()
      .mockRejectedValueOnce(apiError(403, "POW_REQUIRED"))
      .mockResolvedValueOnce(REGISTERED);
    const client = mockClient({ register });
    const onSolvingStart = jest.fn();

    const result = await registerWithPow(client, hardware, PUB_KEY, {
      solve,
      onSolvingStart,
    }).promise;

    expect(result).toEqual(REGISTERED);
    expect(register).toHaveBeenCalledTimes(2);
    expect(register).toHaveBeenNthCalledWith(2, hardware, {
      challengeId: "chal-1",
      nonce: "42",
    });
    expect(client.fetchRegistrationChallenge).toHaveBeenCalledTimes(1);
    expect(calls).toEqual([
      { challengeHex: "ab".repeat(32), publicKey: PUB_KEY, bits: 20 },
    ]);
    expect(onSolvingStart).toHaveBeenCalledWith(20);
  });

  it("fetches a FRESH challenge and retries once more on POW_INVALID", async () => {
    const { solve, calls } = immediateSolver();
    const register = jest
      .fn()
      .mockRejectedValueOnce(apiError(403, "POW_REQUIRED"))
      .mockRejectedValueOnce(apiError(400, "POW_INVALID"))
      .mockResolvedValueOnce(REGISTERED);
    const fetchRegistrationChallenge = jest
      .fn()
      .mockResolvedValueOnce(challenge("chal-1"))
      .mockResolvedValueOnce(challenge("chal-2"));
    const client = mockClient({ register, fetchRegistrationChallenge });

    const result = await registerWithPow(client, hardware, PUB_KEY, { solve }).promise;

    expect(result).toEqual(REGISTERED);
    expect(register).toHaveBeenCalledTimes(3);
    expect(register).toHaveBeenNthCalledWith(3, hardware, {
      challengeId: "chal-2",
      nonce: "19497",
    });
    expect(fetchRegistrationChallenge).toHaveBeenCalledTimes(2);
    expect(calls).toHaveLength(2);
  });

  it("surfaces a second POW_INVALID without a third solve", async () => {
    const { solve, calls } = immediateSolver();
    const secondInvalid = apiError(400, "POW_INVALID");
    const register = jest
      .fn()
      .mockRejectedValueOnce(apiError(403, "POW_REQUIRED"))
      .mockRejectedValueOnce(apiError(400, "POW_INVALID"))
      .mockRejectedValueOnce(secondInvalid);
    const client = mockClient({ register });

    await expect(
      registerWithPow(client, hardware, PUB_KEY, { solve }).promise
    ).rejects.toBe(secondInvalid);
    expect(register).toHaveBeenCalledTimes(3);
    expect(calls).toHaveLength(2);
  });

  it("surfaces non-pow errors from the bare register without solving", async () => {
    const { solve, calls } = immediateSolver();
    const capError = apiError(429, "REGISTRATION_CAP_EXCEEDED");
    const client = mockClient({ register: jest.fn().mockRejectedValue(capError) });

    await expect(
      registerWithPow(client, hardware, PUB_KEY, { solve }).promise
    ).rejects.toBe(capError);
    expect(client.fetchRegistrationChallenge).not.toHaveBeenCalled();
    expect(calls).toHaveLength(0);
  });

  it("surfaces non-POW_INVALID errors from the pow retry without another attempt", async () => {
    const { solve } = immediateSolver();
    const rateLimited = apiError(429, "RATE_LIMITED");
    const register = jest
      .fn()
      .mockRejectedValueOnce(apiError(403, "POW_REQUIRED"))
      .mockRejectedValueOnce(rateLimited);
    const client = mockClient({ register });

    await expect(
      registerWithPow(client, hardware, PUB_KEY, { solve }).promise
    ).rejects.toBe(rateLimited);
    expect(register).toHaveBeenCalledTimes(2);
  });

  it("re-fetches AT MOST once when the challenge expired during the solve", async () => {
    const { solve, calls } = immediateSolver();
    const register = jest
      .fn()
      .mockRejectedValueOnce(apiError(403, "POW_REQUIRED"))
      .mockResolvedValueOnce(REGISTERED);
    // Both challenges are already expired relative to the injected clock: the guard
    // must re-fetch exactly once and then submit anyway (clock-skew bound).
    const fetchRegistrationChallenge = jest
      .fn()
      .mockResolvedValueOnce(challenge("chal-1", "2026-01-01T00:00:00Z"))
      .mockResolvedValueOnce(challenge("chal-2", "2026-01-01T00:00:00Z"));
    const client = mockClient({ register, fetchRegistrationChallenge });

    const result = await registerWithPow(client, hardware, PUB_KEY, {
      solve,
      now: () => Date.parse("2026-07-09T00:00:00Z"),
    }).promise;

    expect(result).toEqual(REGISTERED);
    expect(fetchRegistrationChallenge).toHaveBeenCalledTimes(2);
    expect(calls).toHaveLength(2);
    expect(register).toHaveBeenNthCalledWith(2, hardware, {
      challengeId: "chal-2",
      nonce: "19497",
    });
  });

  it("rejects with PowSolveCancelledError when cancelled mid-solve", async () => {
    const register = jest
      .fn()
      .mockRejectedValueOnce(apiError(403, "POW_REQUIRED"))
      .mockResolvedValueOnce(REGISTERED);
    const client = mockClient({ register });

    // A solver whose promise only settles via cancel().
    let rejectSolve!: (err: Error) => void;
    const cancel = jest.fn(() => rejectSolve(new PowSolveCancelledError()));
    const solve = () => ({
      promise: new Promise<PowSolution>((_resolve, reject) => {
        rejectSolve = reject;
      }),
      cancel,
    });

    const flow = registerWithPow(client, hardware, PUB_KEY, { solve });
    // Let the flow reach the solve before cancelling.
    await new Promise((r) => setTimeout(r, 0));
    flow.cancel();

    await expect(flow.promise).rejects.toBeInstanceOf(PowSolveCancelledError);
    // The bare attempt happened; the pow retry never did.
    expect(register).toHaveBeenCalledTimes(1);
  });

  it("reports solve progress and completion through the hooks", async () => {
    const onSolveProgress = jest.fn();
    const onSolved = jest.fn();
    const solve = (
      _hex: string,
      _key: string,
      _bits: number,
      progress?: (attempts: number) => void
    ) => {
      progress?.(65536);
      return {
        promise: Promise.resolve<PowSolution>({ nonce: "7", attempts: 70000 }),
        cancel: jest.fn(),
      };
    };
    const register = jest
      .fn()
      .mockRejectedValueOnce(apiError(403, "POW_REQUIRED"))
      .mockResolvedValueOnce(REGISTERED);
    const client = mockClient({ register });

    await registerWithPow(client, hardware, PUB_KEY, {
      solve,
      onSolveProgress,
      onSolved,
      now: () => 1000,
    }).promise;

    expect(onSolveProgress).toHaveBeenCalledWith(65536);
    expect(onSolved).toHaveBeenCalledWith(70000, 0);
  });

  it("surfaces challenge-fetch failures unchanged", async () => {
    const { solve } = immediateSolver();
    const fetchFailure = apiError(503, "POW_UNAVAILABLE");
    const register = jest.fn().mockRejectedValueOnce(apiError(403, "POW_REQUIRED"));
    const client = mockClient({
      register,
      fetchRegistrationChallenge: jest.fn().mockRejectedValue(fetchFailure),
    });

    await expect(
      registerWithPow(client, hardware, PUB_KEY, { solve }).promise
    ).rejects.toBe(fetchFailure);
  });
});
