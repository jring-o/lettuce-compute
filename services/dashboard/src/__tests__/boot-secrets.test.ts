import {
  assertProductionSecrets,
  nextAuthSecretProblem,
} from "@/lib/boot-secrets";

// A real `openssl rand -base64 32` value: 44 chars, no placeholder stem.
const VALID_SECRET = "+rtRRGv7j5U86SdedZ72sSs3CnhelPLP3FFysmpKILA=";

describe("assertProductionSecrets", () => {
  it("throws in production when NEXTAUTH_SECRET is missing", () => {
    expect(() => assertProductionSecrets({ NODE_ENV: "production" })).toThrow(
      /boot secret validation: NEXTAUTH_SECRET/,
    );
  });

  it("throws in production for the committed .env.example placeholder", () => {
    expect(() =>
      assertProductionSecrets({
        NODE_ENV: "production",
        NEXTAUTH_SECRET: "generate-with-openssl-rand-base64-32",
      }),
    ).toThrow(/boot secret validation: NEXTAUTH_SECRET/);
  });

  it("throws in production for the dev-compose placeholder", () => {
    expect(() =>
      assertProductionSecrets({
        NODE_ENV: "production",
        NEXTAUTH_SECRET: "dev-secret-not-for-production",
      }),
    ).toThrow(/boot secret validation: NEXTAUTH_SECRET/);
  });

  it("throws in production for a 20-char strong-looking value below the length floor", () => {
    expect(() =>
      assertProductionSecrets({
        NODE_ENV: "production",
        NEXTAUTH_SECRET: "Xk7QpZr2Wm9Lt4Vn8Bd", // 20 chars, no placeholder stem
      }),
    ).toThrow(/boot secret validation: NEXTAUTH_SECRET/);
  });

  it("passes in production for a 44-char generated-shape value", () => {
    expect(() =>
      assertProductionSecrets({
        NODE_ENV: "production",
        NEXTAUTH_SECRET: VALID_SECRET,
      }),
    ).not.toThrow();
  });

  it("passes in production when only AUTH_SECRET is set to a valid value", () => {
    expect(() =>
      assertProductionSecrets({
        NODE_ENV: "production",
        AUTH_SECRET: VALID_SECRET,
      }),
    ).not.toThrow();
  });

  it("does not throw in development even with a placeholder secret", () => {
    expect(() =>
      assertProductionSecrets({
        NODE_ENV: "development",
        NEXTAUTH_SECRET: "generate-with-openssl-rand-base64-32",
      }),
    ).not.toThrow();
  });

  it("does not throw during the next build prerender pass even when the secret is missing", () => {
    expect(() =>
      assertProductionSecrets({
        NODE_ENV: "production",
        NEXT_PHASE: "phase-production-build",
      }),
    ).not.toThrow();
  });
});

describe("nextAuthSecretProblem", () => {
  it("reports missing for undefined", () => {
    expect(nextAuthSecretProblem(undefined)).toBe("missing");
  });

  it("reports missing for a whitespace-only value", () => {
    expect(nextAuthSecretProblem("   ")).toBe("missing");
  });

  it("reports a placeholder for a known stem (case-insensitive)", () => {
    expect(nextAuthSecretProblem("Generate-With-Openssl-Rand-Base64-32")).toBe(
      "is a known placeholder value",
    );
  });

  it("reports the length floor for a short non-placeholder value", () => {
    expect(nextAuthSecretProblem("Xk7QpZr2Wm9Lt4Vn8Bd")).toBe(
      "is shorter than 32 characters",
    );
  });

  it("returns null for a valid generated-shape value", () => {
    expect(nextAuthSecretProblem(VALID_SECRET)).toBeNull();
  });
});
