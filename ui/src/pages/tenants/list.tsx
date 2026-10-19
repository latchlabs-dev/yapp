import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { tenants as tenantsApi } from "@/lib/api";
import { Plus, Building2, MoreHorizontal, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { PageContainer, PageHeading } from "@/components/page";
import { useConfirm } from "@/components/confirm";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription,
  DialogFooter, DialogClose,
} from "@/components/ui/dialog";
import { formatDate } from "@/lib/utils";
import { toast } from "sonner";

export function TenantsListPage() {
  const qc = useQueryClient();
  const confirm = useConfirm();
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState("");

  const { data, isLoading } = useQuery({
    queryKey: ["tenants"],
    queryFn: () => tenantsApi.list(),
  });

  const createMut = useMutation({
    mutationFn: (name: string) => tenantsApi.create({ name }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tenants"] });
      setShowCreate(false);
      setNewName("");
      toast.success("Tenant created");
    },
    onError: (err) => {
      toast.error("Failed to create tenant", { description: err.message });
    },
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => tenantsApi.delete(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tenants"] });
      toast.success("Tenant deleted");
    },
    onError: (err) => {
      toast.error("Failed to delete tenant", { description: err.message });
    },
  });

  return (
    <PageContainer>
      <PageHeading
        title="Tenants"
        description="Manage organizations and their access"
      >
        <Button onClick={() => setShowCreate(true)}>
          <Plus className="h-4 w-4" />
          New Tenant
        </Button>
      </PageHeading>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            {data?.tenants?.length ?? 0} Tenants
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>ID</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="w-12"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoading ? (
                <TableRow>
                  <TableCell colSpan={5} className="text-center py-8 text-muted-foreground">
                    Loading...
                  </TableCell>
                </TableRow>
              ) : data?.tenants?.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="text-center py-8 text-muted-foreground">
                    <Building2 className="mx-auto h-8 w-8 mb-2 opacity-40" />
                    No tenants yet
                  </TableCell>
                </TableRow>
              ) : (
                data?.tenants?.map((t) => (
                  <TableRow key={t.id} className="data-row">
                    <TableCell>
                      <Link
                        to={`/tenants/${t.id}`}
                        className="font-medium hover:text-primary transition-colors"
                      >
                        {t.name}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {t.id}
                    </TableCell>
                    <TableCell>
                      <Badge variant={t.status === "active" ? "success" : t.status === "disabled" ? "destructive" : "default"}>
                        {t.status}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-muted-foreground text-sm">
                      {formatDate(t.created_at)}
                    </TableCell>
                    <TableCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="h-8 w-8">
                            <MoreHorizontal className="h-4 w-4" />
                            <span className="sr-only">Actions</span>
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end" className="min-w-[10rem]">
                          <DropdownMenuItem
                            className="text-destructive focus:text-destructive"
                            onClick={async () => {
                              if (
                                await confirm({
                                  title: "Delete tenant?",
                                  description:
                                    "This deletes the tenant and all associated data. This action cannot be undone.",
                                  confirmLabel: "Delete tenant",
                                  destructive: true,
                                })
                              ) {
                                deleteMut.mutate(t.id);
                              }
                            }}
                          >
                            <Trash2 className="h-4 w-4" />
                            Delete tenant
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Dialog open={showCreate} onOpenChange={setShowCreate}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create Tenant</DialogTitle>
            <DialogDescription>
              Create a new tenant organization for managing users and API keys.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Tenant Name</label>
              <Input
                placeholder="e.g., Acme Corp"
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                autoFocus
              />
            </div>
          </div>
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button
              onClick={() => createMut.mutate(newName)}
              disabled={!newName.trim() || createMut.isPending}
            >
              {createMut.isPending ? "Creating..." : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </PageContainer>
  );
}
