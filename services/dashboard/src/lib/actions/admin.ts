"use server";

import bcrypt from "bcryptjs";
import { eq } from "drizzle-orm";
import { z } from "zod";
import { db } from "@/lib/db";
import { users } from "@/lib/db/schema";
import { type ActionResult, requireAuth } from "./helpers";

function forbiddenError() {
  return { error: { code: "FORBIDDEN", message: "Admin access required." } };
}

function internalError() {
  return { error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." } };
}

async function requireAdmin() {
  const session = await requireAuth();
  if (!session) return null;
  if (session.user.role !== "ADMIN") return null;
  return session;
}

export type UserSummary = {
  id: string;
  username: string;
  email: string;
  displayName: string | null;
  role: string;
  createdAt: Date;
  deactivatedAt: Date | null;
};

const createUserSchema = z.object({
  username: z
    .string()
    .min(3, "Username must be at least 3 characters")
    .max(50, "Username must be at most 50 characters")
    .regex(
      /^[a-z][a-z0-9-]*$/,
      "Username must start with a letter and contain only lowercase letters, numbers, and hyphens",
    )
    .refine((s) => !s.includes("--"), "Username cannot contain consecutive hyphens")
    .refine((s) => !s.endsWith("-"), "Username cannot end with a hyphen"),
  email: z.string().email("Invalid email address"),
  displayName: z.string().max(100).optional(),
  password: z.string().min(8, "Password must be at least 8 characters"),
  role: z.enum(["USER", "ADMIN"]),
});

export async function createUser(data: {
  username: string;
  email: string;
  displayName?: string;
  password: string;
  role: "USER" | "ADMIN";
}): Promise<ActionResult<{ id: string; username: string }>> {
  const session = await requireAdmin();
  if (!session) return forbiddenError();

  const parsed = createUserSchema.safeParse(data);
  if (!parsed.success) {
    const msg = parsed.error.issues.map((i) => i.message).join("; ");
    return { error: { code: "VALIDATION_ERROR", message: msg } };
  }

  const { username, email, displayName, password, role } = parsed.data;
  const passwordHash = await bcrypt.hash(password, 10);

  try {
    const [inserted] = await db
      .insert(users)
      .values({
        email: email.toLowerCase(),
        passwordHash,
        username,
        displayName: displayName || null,
        role,
      })
      .returning({ id: users.id, username: users.username });

    return { data: inserted };
  } catch (err: unknown) {
    if (err instanceof Error && err.message.includes("unique")) {
      if (err.message.includes("email")) {
        return { error: { code: "CONFLICT", message: "A user with this email already exists." } };
      }
      if (err.message.includes("username")) {
        return { error: { code: "CONFLICT", message: "This username is already taken." } };
      }
      return { error: { code: "CONFLICT", message: "A user with this email or username already exists." } };
    }
    return internalError();
  }
}

export async function listUsers(): Promise<ActionResult<UserSummary[]>> {
  const session = await requireAdmin();
  if (!session) return forbiddenError();

  try {
    const rows = await db
      .select({
        id: users.id,
        username: users.username,
        email: users.email,
        displayName: users.displayName,
        role: users.role,
        createdAt: users.createdAt,
        deactivatedAt: users.deactivatedAt,
      })
      .from(users)
      .orderBy(users.createdAt);

    return { data: rows };
  } catch {
    return internalError();
  }
}

export async function deactivateUser(userId: string): Promise<ActionResult<void>> {
  const session = await requireAdmin();
  if (!session) return forbiddenError();

  if (userId === session.user.id) {
    return { error: { code: "FORBIDDEN", message: "You cannot deactivate your own account." } };
  }

  try {
    const [updated] = await db
      .update(users)
      .set({ deactivatedAt: new Date() })
      .where(eq(users.id, userId))
      .returning({ id: users.id });

    if (!updated) {
      return { error: { code: "NOT_FOUND", message: "User not found." } };
    }

    return { data: undefined };
  } catch {
    return internalError();
  }
}

export async function reactivateUser(userId: string): Promise<ActionResult<void>> {
  const session = await requireAdmin();
  if (!session) return forbiddenError();

  try {
    const [updated] = await db
      .update(users)
      .set({ deactivatedAt: null })
      .where(eq(users.id, userId))
      .returning({ id: users.id });

    if (!updated) {
      return { error: { code: "NOT_FOUND", message: "User not found." } };
    }

    return { data: undefined };
  } catch {
    return internalError();
  }
}

export async function updateUserRole(
  userId: string,
  role: "USER" | "ADMIN",
): Promise<ActionResult<void>> {
  const session = await requireAdmin();
  if (!session) return forbiddenError();

  if (userId === session.user.id) {
    return { error: { code: "FORBIDDEN", message: "You cannot change your own role." } };
  }

  try {
    const [updated] = await db
      .update(users)
      .set({ role })
      .where(eq(users.id, userId))
      .returning({ id: users.id });

    if (!updated) {
      return { error: { code: "NOT_FOUND", message: "User not found." } };
    }

    return { data: undefined };
  } catch {
    return internalError();
  }
}

export async function resetUserPassword(
  userId: string,
  newPassword: string,
): Promise<ActionResult<void>> {
  const session = await requireAdmin();
  if (!session) return forbiddenError();

  if (newPassword.length < 8) {
    return { error: { code: "VALIDATION_ERROR", message: "Password must be at least 8 characters." } };
  }

  const passwordHash = await bcrypt.hash(newPassword, 10);

  try {
    const [updated] = await db
      .update(users)
      .set({ passwordHash })
      .where(eq(users.id, userId))
      .returning({ id: users.id });

    if (!updated) {
      return { error: { code: "NOT_FOUND", message: "User not found." } };
    }

    return { data: undefined };
  } catch {
    return internalError();
  }
}
