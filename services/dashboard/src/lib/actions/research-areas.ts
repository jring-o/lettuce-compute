"use server";

import { asc } from "drizzle-orm";

import { db } from "@/lib/db";
import { researchAreas } from "@/lib/db/schema";

export async function listResearchAreas() {
  return db
    .select()
    .from(researchAreas)
    .orderBy(asc(researchAreas.displayOrder));
}
