import Link from "next/link";
import { auth, signOut } from "@/lib/auth";
import { Button } from "@/components/ui/button";
import { MobileMenu } from "./mobile-menu";

export async function Navbar() {
  const session = await auth();
  const user = session?.user ?? null;

  return (
    <nav className="border-b bg-background">
      <div className="mx-auto flex h-16 max-w-7xl items-center justify-between px-4 sm:px-6 lg:px-8">
        <div className="flex items-center gap-6">
          <Link href="/" className="text-lg font-bold tracking-tight">
            Lettuce
          </Link>
          <Link
            href="/leafs"
            className="hidden text-sm text-muted-foreground transition-colors hover:text-foreground sm:block"
          >
            Leafs
          </Link>
          <Link
            href="/contribute"
            className="hidden text-sm text-muted-foreground transition-colors hover:text-foreground sm:block"
          >
            Contribute
          </Link>
        </div>

        {/* Desktop navigation */}
        <div className="hidden items-center gap-3 sm:flex">
          {user ? (
            <>
              <Link
                href="/dashboard/leafs"
                className="text-sm text-muted-foreground transition-colors hover:text-foreground"
              >
                Dashboard
              </Link>
              <Link
                href="/dashboard/settings"
                className="text-sm text-muted-foreground transition-colors hover:text-foreground"
              >
                Settings
              </Link>
              {user.role === "ADMIN" && (
                <Link
                  href="/dashboard/admin/users"
                  className="text-sm text-muted-foreground transition-colors hover:text-foreground"
                >
                  Users
                </Link>
              )}
              <form
                action={async () => {
                  "use server";
                  await signOut({ redirectTo: "/" });
                }}
              >
                <Button variant="outline" size="sm" type="submit">
                  Sign Out
                </Button>
              </form>
            </>
          ) : (
            <Link href="/sign-in">
              <Button variant="ghost" size="sm">
                Sign In
              </Button>
            </Link>
          )}
        </div>

        {/* Mobile navigation */}
        <MobileMenu user={user ? { username: user.username, role: user.role } : null} />
      </div>
    </nav>
  );
}
