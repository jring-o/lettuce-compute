import { z } from "zod";
import { zodFieldErrors } from "@/lib/validations/auth";

function issuesFrom(
  schema: z.ZodType,
  data: unknown,
): z.ZodIssue[] {
  const result = schema.safeParse(data);
  if (result.success) throw new Error("Expected validation to fail");
  return result.error.issues;
}

describe("zodFieldErrors", () => {
  it("returns an empty object for an empty issues array", () => {
    expect(zodFieldErrors([])).toEqual({});
  });

  it("extracts first error per field", () => {
    const schema = z.object({
      email: z.string().email("Invalid email address"),
      password: z.string().min(8, "Password must be at least 8 characters"),
    });

    const errors = zodFieldErrors(
      issuesFrom(schema, { email: "bad", password: "short" }),
    );
    expect(errors.email).toBe("Invalid email address");
    expect(errors.password).toBe("Password must be at least 8 characters");
  });

  it("keeps only the first error when a field has multiple issues", () => {
    // A schema where username can fail two validations
    const schema = z.object({
      username: z
        .string()
        .min(3, "Too short")
        .regex(/^[a-z]/, "Must start with letter"),
    });

    const errors = zodFieldErrors(issuesFrom(schema, { username: "A" }));
    // Should only have one error for username (the first one)
    expect(Object.keys(errors)).toEqual(["username"]);
    expect(typeof errors.username).toBe("string");
  });

  it("handles a single issue", () => {
    const schema = z.object({
      email: z.string().email("Invalid email"),
    });

    const errors = zodFieldErrors(issuesFrom(schema, { email: "nope" }));
    expect(errors).toEqual({ email: "Invalid email" });
  });

  it("handles issues from multiple distinct fields", () => {
    const schema = z.object({
      email: z.string().email("Bad email"),
      password: z.string().min(8, "Too short"),
      username: z.string().min(3, "Name too short"),
    });

    const errors = zodFieldErrors(
      issuesFrom(schema, { email: "x", password: "y", username: "z" }),
    );
    expect(errors.email).toBe("Bad email");
    expect(errors.password).toBe("Too short");
    expect(errors.username).toBe("Name too short");
  });
});
