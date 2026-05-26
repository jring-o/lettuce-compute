export default function AuthLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-[calc(100vh-4rem)] items-center justify-center px-4">
      <div className="w-full max-w-md">
        <div className="mb-8 text-center">
          <h1 className="text-2xl font-bold tracking-tight">Lettuce</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Distributed Volunteer Compute Platform
          </p>
        </div>
        {children}
      </div>
    </div>
  );
}
