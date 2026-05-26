import { db } from "@/lib/db";
import { sql } from "drizzle-orm";
import type { DatabaseStatus, PlatformStatus } from "@/types";

export const dynamic = "force-dynamic";

export default async function HealthPage() {
  let dbStatus: DatabaseStatus = "disconnected";

  try {
    await db.execute(sql`SELECT 1`);
    dbStatus = "connected";
  } catch {
    // dbStatus remains "disconnected"
  }

  const platformStatus: PlatformStatus =
    dbStatus === "connected" ? "healthy" : "unhealthy";
  const timestamp = new Date().toISOString();

  return (
    <div className="flex min-h-screen items-center justify-center">
      <div className="rounded-lg border p-8">
        <h1 className="text-2xl font-bold">Platform Health</h1>
        <dl className="mt-4 space-y-2">
          <div className="flex gap-2">
            <dt className="font-medium">Status:</dt>
            <dd>{platformStatus}</dd>
          </div>
          <div className="flex gap-2">
            <dt className="font-medium">Database:</dt>
            <dd>{dbStatus}</dd>
          </div>
          <div className="flex gap-2">
            <dt className="font-medium">Timestamp:</dt>
            <dd>{timestamp}</dd>
          </div>
        </dl>
      </div>
    </div>
  );
}
