import { getTableName, getTableColumns } from "drizzle-orm";
import {
  users,
  sessions,
} from "@/lib/db/schema";

describe("Drizzle Schema", () => {
  it("defines the users table", () => {
    expect(getTableName(users)).toBe("users");
  });

  it("defines the sessions table", () => {
    expect(getTableName(sessions)).toBe("sessions");
  });

  describe("users columns", () => {
    const cols = getTableColumns(users);

    it("has id, email, and username columns", () => {
      expect(cols.id).toBeDefined();
      expect(cols.email).toBeDefined();
      expect(cols.username).toBeDefined();
    });

    it("has auth-related columns", () => {
      expect(cols.passwordHash).toBeDefined();
    });

    it("does not have dead social login columns (Beta feature)", () => {
      expect((cols as Record<string, unknown>).githubId).toBeUndefined();
      expect((cols as Record<string, unknown>).googleId).toBeUndefined();
      expect((cols as Record<string, unknown>).avatarUrl).toBeUndefined();
    });

    it("has timestamp columns", () => {
      expect(cols.createdAt).toBeDefined();
      expect(cols.updatedAt).toBeDefined();
    });

    it("has deactivatedAt column for user deactivation", () => {
      expect(cols.deactivatedAt).toBeDefined();
    });

    it("deactivatedAt is nullable (active users have null)", () => {
      // Column exists and is not marked notNull — nullable by default
      expect(cols.deactivatedAt).toBeDefined();
      expect(cols.deactivatedAt.notNull).toBe(false);
    });
  });

  describe("sessions columns", () => {
    const cols = getTableColumns(sessions);

    it("has userId, sessionToken, and expires columns", () => {
      expect(cols.userId).toBeDefined();
      expect(cols.sessionToken).toBeDefined();
      expect(cols.expires).toBeDefined();
    });
  });
});
