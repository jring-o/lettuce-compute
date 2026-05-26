import { Card, CardContent, CardFooter, CardHeader } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

export function ProjectCardSkeleton() {
  return (
    <Card className="h-full" data-testid="project-card-skeleton">
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <Skeleton className="h-5 w-3/4" />
          <Skeleton className="h-5 w-14 rounded-full" />
        </div>
        <Skeleton className="mt-1 h-5 w-20 rounded-full" />
      </CardHeader>

      <CardContent className="space-y-3">
        <div className="space-y-1.5">
          <Skeleton className="h-3.5 w-full" />
          <Skeleton className="h-3.5 w-5/6" />
          <Skeleton className="h-3.5 w-2/3" />
        </div>
        <Skeleton className="h-3.5 w-24" />
      </CardContent>

      <CardFooter className="gap-4">
        <Skeleton className="h-3.5 w-10" />
        <Skeleton className="h-2 flex-1 rounded-full" />
        <Skeleton className="h-3.5 w-8" />
      </CardFooter>
    </Card>
  );
}
