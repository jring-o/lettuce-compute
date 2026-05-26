import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { listApiKeys } from "@/lib/actions/api-keys";
import { ApiKeyTable } from "@/components/settings/api-key-table";
import { CreateApiKeyForm } from "@/components/settings/create-api-key-form";

export const metadata = {
  title: "Settings — Lettuce",
};

export default async function SettingsPage() {
  const session = await auth();
  if (!session?.user) redirect("/sign-in");

  const result = await listApiKeys();
  const keys = "data" in result ? result.data : [];

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">Settings</h1>
        <p className="mt-1 text-muted-foreground">
          Manage your account and API keys.
        </p>
      </div>

      <div className="mt-8">
        <div className="flex items-center justify-between">
          <div>
            <h2 className="text-lg font-semibold">API Keys</h2>
            <p className="mt-0.5 text-sm text-muted-foreground">
              Use API keys to authenticate with the Lettuce infrastructure API.
            </p>
          </div>
          <CreateApiKeyForm />
        </div>

        <div className="mt-4">
          <ApiKeyTable keys={keys} />
        </div>
      </div>
    </div>
  );
}
