import { signInSchema } from "@/lib/validations/auth";

describe("signInSchema", () => {
  it("accepts valid sign-in input", () => {
    const result = signInSchema.safeParse({
      email: "user@example.com",
      password: "password123",
    });
    expect(result.success).toBe(true);
  });

  it("rejects invalid email", () => {
    const result = signInSchema.safeParse({
      email: "not-email",
      password: "password123",
    });
    expect(result.success).toBe(false);
  });

  it("rejects empty password", () => {
    const result = signInSchema.safeParse({
      email: "user@example.com",
      password: "",
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      expect(result.error.issues[0].message).toContain("required");
    }
  });

  it("accepts password with a single character (no min length like register)", () => {
    const result = signInSchema.safeParse({
      email: "user@example.com",
      password: "x",
    });
    expect(result.success).toBe(true);
  });

  it("rejects missing email field", () => {
    const result = signInSchema.safeParse({ password: "password123" });
    expect(result.success).toBe(false);
  });

  it("rejects missing password field", () => {
    const result = signInSchema.safeParse({ email: "user@example.com" });
    expect(result.success).toBe(false);
  });
});
