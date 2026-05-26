import Link from "next/link";
import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <div className="flex min-h-[calc(100vh-4rem)] items-center justify-center">
      <div className="text-center">
        <h1 className="text-4xl font-bold tracking-tight">404</h1>
        <p className="mt-2 text-lg text-muted-foreground">Page not found</p>
        <div className="mt-6 flex items-center justify-center gap-4">
          <Link href="/">
            <Button>Home</Button>
          </Link>
          <Link href="/leafs">
            <Button variant="outline">Browse Leafs</Button>
          </Link>
        </div>
      </div>
    </div>
  );
}
