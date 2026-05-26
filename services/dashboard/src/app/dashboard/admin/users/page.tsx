import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { listUsers } from "@/lib/actions/admin";
import { UserTable } from "@/components/admin/user-table";
import { CreateUserForm } from "@/components/admin/create-user-form";

export const metadata = {
  title: "User Management — Lettuce",
};

export default async function AdminUsersPage() {
  const session = await auth();
  if (!session?.user) redirect("/sign-in");
  if (session.user.role !== "ADMIN") redirect("/dashboard/leafs");

  const result = await listUsers();
  const users = "data" in result ? result.data : [];

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">User Management</h1>
          <p className="mt-1 text-muted-foreground">
            Create and manage user accounts on this server.
          </p>
        </div>
        <CreateUserForm />
      </div>

      <div className="mt-8">
        <UserTable users={users} currentUserId={session.user.id} />
      </div>
    </div>
  );
}
