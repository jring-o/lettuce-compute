import Link from "next/link";
import { redirect } from "next/navigation";

import { auth } from "@/lib/auth";
import { listMyLeafs } from "@/lib/actions/projects";
import { Badge } from "@/components/ui/badge";
import { LEAF_STATE_VARIANTS } from "@/lib/utils";

export const metadata = {
  title: "Your Leafs — Lettuce",
};

export default async function DashboardLeafsPage() {
  const session = await auth();
  if (!session?.user) redirect("/sign-in");

  const result = await listMyLeafs();
  const leafs = "data" in result ? result.data.data : [];

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">Your Leafs</h1>
        <p className="mt-1 text-muted-foreground">
          Manage your distributed compute leafs.
        </p>
      </div>

      {leafs.length === 0 ? (
        <div className="mt-16 text-center">
          <p className="text-lg text-muted-foreground">
            No leafs yet. Create one via the infrastructure API.
          </p>
        </div>
      ) : (
        <div className="mt-6 space-y-3">
          {leafs.map((leaf) => (
            <Link
              key={leaf.id}
              href={`/dashboard/leafs/${leaf.slug}`}
              className="flex items-center justify-between rounded-lg border border-border p-4 transition-colors hover:bg-muted"
            >
              <div>
                <h3 className="font-medium">{leaf.name}</h3>
                <p className="mt-0.5 text-sm text-muted-foreground line-clamp-1">
                  {leaf.description}
                </p>
              </div>
              <div className="ml-4 flex items-center gap-3">
                <Badge variant={LEAF_STATE_VARIANTS[leaf.state] ?? "secondary"}>
                  {leaf.state}
                </Badge>
                <span className="text-xs text-muted-foreground">
                  {new Date(leaf.created_at).toLocaleDateString()}
                </span>
              </div>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
