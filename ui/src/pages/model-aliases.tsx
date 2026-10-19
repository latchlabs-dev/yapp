import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { modelAliases as syntheticModelsApi, modelCatalog } from "@/lib/api";
import { useTenantNames } from "@/hooks/use-queries";
import type { ModelAlias } from "@/lib/types";
import { ArrowRight, Network, Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { PageContainer, PageHeading } from "@/components/page";
import { Card, CardContent } from "@/components/ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription,
  DialogFooter, DialogClose,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { formatDate } from "@/lib/utils";
import { toast } from "sonner";

interface TargetOption {
  key: string;
  providerAccountID: string;
  providerName: string;
  modelID: string;
  modelName: string;
}

function SyntheticModelsTable({
  models,
  tenantName,
  providerName,
  onDelete,
}: {
  models: ModelAlias[];
  tenantName: (tenantID?: string) => string;
  providerName: (providerAccountID?: string) => string;
  onDelete: (id: string) => void;
}) {
  if (models.length === 0) {
    return (
      <div className="py-10 text-center text-muted-foreground">
        <Network className="mx-auto h-8 w-8 mb-2 opacity-40" />
        <p>No synthetic models yet.</p>
        <p className="text-xs mt-1">Create a stable model name backed by any active catalog model.</p>
      </div>
    );
  }

  return (
    <Card>
      <CardContent className="p-0">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Synthetic Model</TableHead>
              <TableHead className="w-6" />
              <TableHead>Target Model</TableHead>
              <TableHead>Provider Account</TableHead>
              <TableHead>Scope</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-12" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {models.map((model) => (
              <TableRow key={model.id} className="data-row">
                <TableCell><code className="font-mono text-xs">{model.public_model}</code></TableCell>
                <TableCell className="text-muted-foreground"><ArrowRight className="h-3.5 w-3.5" /></TableCell>
                <TableCell><code className="font-mono text-xs">{model.upstream_model}</code></TableCell>
                <TableCell className="text-sm">{providerName(model.provider_account_id)}</TableCell>
                <TableCell>
                  <Badge variant={model.tenant_id ? "outline" : "info"}>
                    {tenantName(model.tenant_id)}
                  </Badge>
                </TableCell>
                <TableCell>
                  <Badge variant={model.status === "active" ? "success" : "destructive"}>{model.status}</Badge>
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">{formatDate(model.created_at)}</TableCell>
                <TableCell>
                  <Button variant="ghost" size="icon" aria-label="Delete synthetic model" onClick={() => onDelete(model.id)}>
                    <Trash2 className="h-3.5 w-3.5 text-muted-foreground hover:text-destructive" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}

export function ModelAliasesPage() {
  const qc = useQueryClient();
  const { tenants } = useTenantNames();
  const [showCreate, setShowCreate] = useState(false);
  const [isGlobal, setIsGlobal] = useState(true);
  const [name, setName] = useState("");
  const [tenantID, setTenantID] = useState("");
  const [targetKey, setTargetKey] = useState("");

  const { data: aliases } = useQuery({
    queryKey: ["model-aliases", "all"],
    queryFn: () => syntheticModelsApi.listAll(),
  });
  const { data: catalog } = useQuery({
    queryKey: ["model-catalog"],
    queryFn: () => modelCatalog.get(),
  });

  const targets: TargetOption[] = (catalog?.providers ?? []).flatMap((provider) =>
    provider.route_id
      ? provider.models.map((model) => ({
          key: `${provider.route_id}\u001f${model.id}`,
          providerAccountID: provider.route_id!,
          providerName: provider.name,
          modelID: model.id,
          modelName: model.name,
        }))
      : [],
  );
  const selectedTarget = targets.find((target) => target.key === targetKey);

  const resetForm = () => {
    setName("");
    setTenantID("");
    setTargetKey("");
    setIsGlobal(true);
  };

  const createMut = useMutation({
    mutationFn: () => {
      if (!selectedTarget) throw new Error("Choose a target model");
      const payload: Partial<ModelAlias> = {
        public_model: name.trim(),
        provider_account_id: selectedTarget.providerAccountID,
        upstream_model: selectedTarget.modelID,
      };
      return isGlobal
        ? syntheticModelsApi.createGlobal(payload)
        : syntheticModelsApi.createTenant(tenantID, payload);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["model-aliases"] });
      setShowCreate(false);
      resetForm();
      toast.success("Synthetic model created");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => syntheticModelsApi.delete(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["model-aliases"] });
      toast.success("Synthetic model deleted");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  const providerNames = new Map((catalog?.providers ?? []).map((provider) => [provider.route_id, provider.name]));
  const tenantNames = new Map(tenants.map((tenant) => [tenant.id, tenant.name]));

  return (
    <PageContainer>
      <PageHeading
        title="Synthetic Models"
        description="Publish stable model names that route to active provider models"
      >
        <Button onClick={() => setShowCreate(true)}>
          <Plus className="h-4 w-4" />
          New Synthetic Model
        </Button>
      </PageHeading>

      <SyntheticModelsTable
        models={aliases?.model_aliases ?? []}
        tenantName={(id) => id ? tenantNames.get(id) ?? id : "Global"}
        providerName={(id) => providerNames.get(id) ?? id ?? "Unknown"}
        onDelete={(id) => deleteMut.mutate(id)}
      />

      <Dialog open={showCreate} onOpenChange={(open) => { setShowCreate(open); if (!open) resetForm(); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create Synthetic Model</DialogTitle>
            <DialogDescription>
              The new name appears in model discovery and routes explicitly to the selected catalog model.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Model Name</label>
              <Input
                placeholder="standard-flash"
                value={name}
                onChange={(event) => setName(event.target.value)}
                className="font-mono"
              />
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium">Routes To</label>
              <Select value={targetKey} onValueChange={setTargetKey}>
                <SelectTrigger><SelectValue placeholder="Select an active provider model" /></SelectTrigger>
                <SelectContent>
                  {targets.map((target) => (
                    <SelectItem key={target.key} value={target.key}>
                      {target.providerName} / {target.modelName} ({target.modelID})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={isGlobal}
                onChange={(event) => setIsGlobal(event.target.checked)}
                className="rounded"
              />
              Global (available to every tenant)
            </label>
            {!isGlobal && (
              <div className="space-y-2">
                <label className="text-sm font-medium">Tenant</label>
                <Select value={tenantID} onValueChange={setTenantID}>
                  <SelectTrigger><SelectValue placeholder="Select a tenant" /></SelectTrigger>
                  <SelectContent>
                    {tenants.map((tenant) => <SelectItem key={tenant.id} value={tenant.id}>{tenant.name}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
            )}
          </div>
          <DialogFooter>
            <DialogClose asChild><Button variant="outline">Cancel</Button></DialogClose>
            <Button
              onClick={() => createMut.mutate()}
              disabled={!name.trim() || !selectedTarget || (!isGlobal && !tenantID) || createMut.isPending}
            >
              {createMut.isPending ? "Creating..." : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </PageContainer>
  );
}
