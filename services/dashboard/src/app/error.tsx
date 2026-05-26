"use client";

import { Button } from "@/components/ui/button";

export default function Error({
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <div className="flex min-h-[calc(100vh-4rem)] items-center justify-center">
      <div className="text-center">
        <h1 className="text-2xl font-bold tracking-tight">
          Something went wrong
        </h1>
        <p className="mt-2 text-muted-foreground">
          An error occurred while loading this page. The infrastructure server
          may be temporarily unavailable.
        </p>
        <Button className="mt-6" onClick={reset}>
          Try Again
        </Button>
      </div>
    </div>
  );
}
