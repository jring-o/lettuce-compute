import Link from "next/link";
import { auth } from "@/lib/auth";
import { Button } from "@/components/ui/button";

export default async function Home() {
  const session = await auth();

  return (
    <div className="flex min-h-[calc(100vh-4rem)] items-center justify-center">
      <div className="text-center">
        <h1 className="text-4xl font-bold tracking-tight">Lettuce</h1>
        <p className="mt-4 text-lg text-muted-foreground">
          Distributed Volunteer Compute Platform
        </p>
        <div className="mt-8 flex items-center justify-center gap-4">
          <Link href="/leafs">
            <Button size="lg">Browse Leafs</Button>
          </Link>
          {session?.user ? (
            <Link href="/dashboard/leafs">
              <Button variant="outline" size="lg">
                Dashboard
              </Button>
            </Link>
          ) : (
            <Link href="/sign-in">
              <Button variant="outline" size="lg">
                Admin Sign In
              </Button>
            </Link>
          )}
        </div>
      </div>
    </div>
  );
}
