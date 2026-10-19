import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, Cable, Pencil, Plus, Sparkles, Tags, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  insightConfigurations,
  insightProcessors,
  insightRuns,
  insightTags,
} from "@/lib/api";
import type { InsightConfiguration, InsightProcessor, InsightTag } from "@/lib/types";
import { useTenantNames } from "@/hooks/use-queries";
import { formatDate } from "@/lib/utils";
import { PageContainer, PageHeading, EmptyState } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

type ProcessorForm = {
  id?: string;
  name: string;
  grpc_target: string;
  plaintext: boolean;
  tls_server_name: string;
  timeout_ms: number;
  status: "active" | "disabled";
};

type ConfigurationForm = {
  id?: string;
  name: string;
  processor_id: string;
  scope: "global" | "tenant" | "project";
  tenant_id: string;
  project_id: string;
  instructions: string;
  allow_new_tags: boolean;
  include_session_summary: boolean;
  include_insight_summaries: boolean;
  max_input_items: number;
  max_input_chars: number;
  status: "active" | "disabled";
};

const emptyProcessor = (): ProcessorForm => ({
  name: "", grpc_target: "piproxy-insights:50051", plaintext: true,
  tls_server_name: "", timeout_ms: 120000, status: "active",
});

const emptyConfiguration = (processorId = ""): ConfigurationForm => ({
  name: "", processor_id: processorId, scope: "global", tenant_id: "", project_id: "",
  instructions: "", allow_new_tags: true, include_session_summary: true,
  include_insight_summaries: true, max_input_items: 1000, max_input_chars: 500000,
  status: "active",
});

export function InsightsPage() {
  const queryClient = useQueryClient();
  const { tenants, tenantName } = useTenantNames();
  const [processorForm, setProcessorForm] = useState<ProcessorForm | null>(null);
  const [configurationForm, setConfigurationForm] = useState<ConfigurationForm | null>(null);
  const [tagForm, setTagForm] = useState<{ originalName?: string; name: string; description: string } | null>(null);

  const processorsQuery = useQuery({ queryKey: ["insight-processors"], queryFn: insightProcessors.list });
  const configurationsQuery = useQuery({ queryKey: ["insight-configurations"], queryFn: insightConfigurations.list });
  const tagsQuery = useQuery({ queryKey: ["insight-tags"], queryFn: insightTags.list });
  const runsQuery = useQuery({ queryKey: ["insight-runs"], queryFn: () => insightRuns.list(100), refetchInterval: 30000 });
  const processors = processorsQuery.data?.insight_processors ?? [];
  const configurations = configurationsQuery.data?.insight_configurations ?? [];
  const tags = tagsQuery.data?.insight_tags ?? [];
  const runs = runsQuery.data?.insight_runs ?? [];

  const refresh = (key: string) => queryClient.invalidateQueries({ queryKey: [key] });

  const saveProcessor = useMutation({
    mutationFn: (form: ProcessorForm) => {
      const payload = {
        name: form.name, grpc_target: form.grpc_target, plaintext: form.plaintext,
        tls_server_name: form.tls_server_name, timeout_ms: form.timeout_ms, status: form.status,
      };
      return form.id ? insightProcessors.update(form.id, payload) : insightProcessors.create(payload);
    },
    onSuccess: () => { refresh("insight-processors"); refresh("insight-configurations"); setProcessorForm(null); toast.success("Insight service saved"); },
    onError: (error) => toast.error("Could not save service", { description: error.message }),
  });
  const disableProcessor = useMutation({
    mutationFn: insightProcessors.disable,
    onSuccess: () => { refresh("insight-processors"); refresh("insight-configurations"); toast.success("Insight service disabled"); },
    onError: (error) => toast.error("Could not disable service", { description: error.message }),
  });
  const testProcessor = useMutation({
    mutationFn: insightProcessors.test,
    onSuccess: (health) => toast.success(`${health.service} is healthy`, { description: health.version ? `Version ${health.version}` : undefined }),
    onError: (error) => toast.error("gRPC connection failed", { description: error.message }),
  });

  const saveConfiguration = useMutation({
    mutationFn: (form: ConfigurationForm) => {
      const payload = {
        name: form.name, processor_id: form.processor_id,
        tenant_id: form.scope === "global" ? "" : form.tenant_id,
        project_id: form.scope === "project" ? form.project_id : "",
        instructions: form.instructions, allow_new_tags: form.allow_new_tags,
        include_session_summary: form.include_session_summary,
        include_insight_summaries: form.include_insight_summaries,
        max_input_items: form.max_input_items, max_input_chars: form.max_input_chars,
        status: form.status,
      };
      return form.id ? insightConfigurations.update(form.id, payload) : insightConfigurations.create(payload);
    },
    onSuccess: () => { refresh("insight-configurations"); setConfigurationForm(null); toast.success("Insight configuration saved"); },
    onError: (error) => toast.error("Could not save configuration", { description: error.message }),
  });
  const disableConfiguration = useMutation({
    mutationFn: insightConfigurations.disable,
    onSuccess: () => { refresh("insight-configurations"); toast.success("Insight configuration disabled"); },
    onError: (error) => toast.error("Could not disable configuration", { description: error.message }),
  });

  const saveTag = useMutation({
    mutationFn: (form: { originalName?: string; name: string; description: string }) => insightTags.upsert({ name: form.name, description: form.description, status: "active" }),
    onSuccess: () => { refresh("insight-tags"); setTagForm(null); toast.success("Tag saved"); },
    onError: (error) => toast.error("Could not save tag", { description: error.message }),
  });
  const disableTag = useMutation({
    mutationFn: insightTags.disable,
    onSuccess: () => { refresh("insight-tags"); toast.success("Tag disabled"); },
    onError: (error) => toast.error("Could not disable tag", { description: error.message }),
  });

  const editProcessor = (processor: InsightProcessor) => setProcessorForm({
    id: processor.id, name: processor.name, grpc_target: processor.grpc_target,
    plaintext: processor.plaintext, tls_server_name: processor.tls_server_name ?? "",
    timeout_ms: processor.timeout_ms, status: processor.status,
  });
  const editConfiguration = (config: InsightConfiguration) => setConfigurationForm({
    id: config.id, name: config.name, processor_id: config.processor_id,
    scope: config.project_id ? "project" : config.tenant_id ? "tenant" : "global",
    tenant_id: config.tenant_id ?? "", project_id: config.project_id ?? "",
    instructions: config.instructions, allow_new_tags: config.allow_new_tags,
    include_session_summary: config.include_session_summary,
    include_insight_summaries: config.include_insight_summaries,
    max_input_items: config.max_input_items, max_input_chars: config.max_input_chars,
    status: config.status,
  });

  return (
    <PageContainer>
      <PageHeading title="Insights" description="Run multiple insight policies through independently deployed gRPC services." />
      <Tabs defaultValue="configurations">
        <TabsList>
          <TabsTrigger value="configurations"><Sparkles className="mr-1 h-4 w-4" /> Configurations</TabsTrigger>
          <TabsTrigger value="services"><Cable className="mr-1 h-4 w-4" /> Services</TabsTrigger>
          <TabsTrigger value="tags"><Tags className="mr-1 h-4 w-4" /> Tags</TabsTrigger>
          <TabsTrigger value="runs"><Activity className="mr-1 h-4 w-4" /> Runs</TabsTrigger>
        </TabsList>

        <TabsContent value="configurations" className="mt-4 space-y-4">
          <div className="flex items-center justify-between">
            <p className="text-sm text-muted-foreground">Every matching active configuration runs independently for a session.</p>
            <Button onClick={() => setConfigurationForm(emptyConfiguration(processors.find((p) => p.status === "active")?.id))} disabled={processors.length === 0}>
              <Plus className="h-4 w-4" /> New configuration
            </Button>
          </div>
          {configurations.length === 0 ? (
            <Card><EmptyState icon={Sparkles} title="No insight configurations" description="Add a service, then create one or more policies for global, tenant, or project scope." /></Card>
          ) : (
            <Card><CardContent className="p-0"><Table><TableHeader><TableRow>
              <TableHead>Name</TableHead><TableHead>Service</TableHead><TableHead>Scope</TableHead><TableHead>Tags & summaries</TableHead><TableHead>Revision</TableHead><TableHead>Status</TableHead><TableHead className="w-24" />
            </TableRow></TableHeader><TableBody>{configurations.map((config) => (
              <TableRow key={config.id}>
                <TableCell><div className="font-medium">{config.name}</div><div className="max-w-sm truncate text-xs text-muted-foreground">{config.instructions || "No extra instructions"}</div></TableCell>
                <TableCell>{config.processor?.name ?? config.processor_id}</TableCell>
                <TableCell>{config.project_id ? <Badge variant="outline">Project · {config.project_id}</Badge> : config.tenant_id ? <Badge variant="outline">Tenant · {tenantName(config.tenant_id)}</Badge> : <Badge>Global</Badge>}</TableCell>
                <TableCell className="text-xs text-muted-foreground">{config.allow_new_tags ? "May add tags" : "Current tags only"}<br />{config.include_session_summary && "Session summary"}{config.include_session_summary && config.include_insight_summaries && " + "}{config.include_insight_summaries && "Insight summaries"}</TableCell>
                <TableCell>v{config.revision}</TableCell><TableCell><StatusBadge status={config.status} /></TableCell>
                <TableCell><div className="flex"><Button variant="ghost" size="icon" aria-label="Edit configuration" onClick={() => editConfiguration(config)}><Pencil className="h-4 w-4" /></Button><Button variant="ghost" size="icon" aria-label="Disable configuration" onClick={() => disableConfiguration.mutate(config.id)} disabled={config.status === "disabled"}><Trash2 className="h-4 w-4" /></Button></div></TableCell>
              </TableRow>
            ))}</TableBody></Table></CardContent></Card>
          )}
        </TabsContent>

        <TabsContent value="services" className="mt-4 space-y-4">
          <div className="flex items-center justify-between"><p className="text-sm text-muted-foreground">Persistent HTTP/2 connections are reused by the proxy and admin health checks.</p><Button onClick={() => setProcessorForm(emptyProcessor())}><Plus className="h-4 w-4" /> New service</Button></div>
          <Card><CardContent className="p-0">{processors.length === 0 ? <EmptyState icon={Cable} title="No insight services" description="Register the gRPC target of an independently deployed insights service." /> : <Table><TableHeader><TableRow><TableHead>Name</TableHead><TableHead>gRPC target</TableHead><TableHead>Transport</TableHead><TableHead>Timeout</TableHead><TableHead>Status</TableHead><TableHead className="w-52" /></TableRow></TableHeader><TableBody>{processors.map((processor) => (
            <TableRow key={processor.id}><TableCell className="font-medium">{processor.name}</TableCell><TableCell><code className="text-xs">{processor.grpc_target}</code></TableCell><TableCell>{processor.plaintext ? "Plaintext" : `TLS${processor.tls_server_name ? ` · ${processor.tls_server_name}` : ""}`}</TableCell><TableCell>{Math.round(processor.timeout_ms / 1000)}s</TableCell><TableCell><StatusBadge status={processor.status} /></TableCell><TableCell><div className="flex justify-end gap-1"><Button variant="outline" size="sm" onClick={() => testProcessor.mutate(processor.id)}>Test</Button><Button variant="ghost" size="icon" aria-label="Edit service" onClick={() => editProcessor(processor)}><Pencil className="h-4 w-4" /></Button><Button variant="ghost" size="icon" aria-label="Disable service" onClick={() => disableProcessor.mutate(processor.id)} disabled={processor.status === "disabled"}><Trash2 className="h-4 w-4" /></Button></div></TableCell></TableRow>
          ))}</TableBody></Table>}</CardContent></Card>
        </TabsContent>

        <TabsContent value="tags" className="mt-4 space-y-4">
          <div className="flex items-center justify-between"><p className="text-sm text-muted-foreground">Current tags are sent to every model. Allowed models may propose additions.</p><Button onClick={() => setTagForm({ name: "", description: "" })}><Plus className="h-4 w-4" /> Add tag</Button></div>
          <Card><CardContent className="p-0"><Table><TableHeader><TableRow><TableHead>Tag</TableHead><TableHead>Description</TableHead><TableHead>Uses</TableHead><TableHead>Status</TableHead><TableHead className="w-24" /></TableRow></TableHeader><TableBody>{tags.map((tag: InsightTag) => (
            <TableRow key={tag.name}><TableCell><Badge variant="outline">{tag.name}</Badge></TableCell><TableCell>{tag.description || <span className="text-muted-foreground">No description</span>}</TableCell><TableCell>{tag.usage_count}</TableCell><TableCell><StatusBadge status={tag.status} /></TableCell><TableCell><div className="flex"><Button variant="ghost" size="icon" aria-label="Edit tag" onClick={() => setTagForm({ originalName: tag.name, name: tag.name, description: tag.description })}><Pencil className="h-4 w-4" /></Button><Button variant="ghost" size="icon" aria-label="Disable tag" onClick={() => disableTag.mutate(tag.name)} disabled={tag.status === "disabled"}><Trash2 className="h-4 w-4" /></Button></div></TableCell></TableRow>
          ))}</TableBody></Table></CardContent></Card>
        </TabsContent>

        <TabsContent value="runs" className="mt-4">
          <Card><CardHeader><CardTitle className="text-base">Recent insight runs</CardTitle></CardHeader><CardContent className="p-0">{runs.length === 0 ? <EmptyState icon={Activity} title="No runs yet" description="Runs appear after a matching session reaches the nightly insight queue." /> : <Table><TableHeader><TableRow><TableHead>Started</TableHead><TableHead>Configuration</TableHead><TableHead>Project</TableHead><TableHead>Session summary</TableHead><TableHead>Insights</TableHead><TableHead>Status</TableHead></TableRow></TableHeader><TableBody>{runs.map((run) => (
            <TableRow key={run.id}><TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDate(run.started_at)}</TableCell><TableCell>{run.configuration_name ?? run.configuration_id}<div className="text-xs text-muted-foreground">v{run.configuration_revision}</div></TableCell><TableCell><code className="text-xs">{run.project_id}</code></TableCell><TableCell className="max-w-md whitespace-pre-line text-sm">{run.session_summary || run.error || "—"}</TableCell><TableCell>{run.insight_count}</TableCell><TableCell><StatusBadge status={run.status} /></TableCell></TableRow>
          ))}</TableBody></Table>}</CardContent></Card>
        </TabsContent>
      </Tabs>

      <ProcessorDialog form={processorForm} setForm={setProcessorForm} onSave={() => processorForm && saveProcessor.mutate(processorForm)} saving={saveProcessor.isPending} />
      <ConfigurationDialog form={configurationForm} setForm={setConfigurationForm} processors={processors} tenants={tenants} onSave={() => configurationForm && saveConfiguration.mutate(configurationForm)} saving={saveConfiguration.isPending} />
      <TagDialog form={tagForm} setForm={setTagForm} onSave={() => tagForm && saveTag.mutate(tagForm)} saving={saveTag.isPending} />
    </PageContainer>
  );
}

function StatusBadge({ status }: { status: string }) {
  return <Badge variant={status === "active" || status === "completed" ? "success" : status === "failed" ? "destructive" : "outline"}>{status}</Badge>;
}

function ProcessorDialog({ form, setForm, onSave, saving }: { form: ProcessorForm | null; setForm: (form: ProcessorForm | null) => void; onSave: () => void; saving: boolean }) {
  if (!form) return null;
  return <Dialog open onOpenChange={(open) => !open && setForm(null)}><DialogContent><DialogHeader><DialogTitle>{form.id ? "Edit" : "Add"} insight service</DialogTitle><DialogDescription>The target is independently deployed and reached over gRPC.</DialogDescription></DialogHeader><div className="space-y-4 py-2">
    <Field label="Name"><Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="Primary insights" /></Field>
    <Field label="gRPC target"><Input value={form.grpc_target} onChange={(event) => setForm({ ...form, grpc_target: event.target.value })} placeholder="insights.internal:50051" /></Field>
    <div className="grid grid-cols-2 gap-4"><Field label="Timeout (ms)"><Input type="number" min={1000} value={form.timeout_ms} onChange={(event) => setForm({ ...form, timeout_ms: Number(event.target.value) })} /></Field><Field label="Status"><Select value={form.status} onValueChange={(value: "active" | "disabled") => setForm({ ...form, status: value })}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent><SelectItem value="active">Active</SelectItem><SelectItem value="disabled">Disabled</SelectItem></SelectContent></Select></Field></div>
    <Toggle label="Plaintext transport" description="Use only on a trusted private network." checked={form.plaintext} onChange={(checked) => setForm({ ...form, plaintext: checked })} />
    {!form.plaintext && <Field label="TLS server name (optional)"><Input value={form.tls_server_name} onChange={(event) => setForm({ ...form, tls_server_name: event.target.value })} /></Field>}
  </div><DialogFooter><Button variant="outline" onClick={() => setForm(null)}>Cancel</Button><Button onClick={onSave} disabled={saving || !form.name.trim() || !form.grpc_target.trim()}>Save service</Button></DialogFooter></DialogContent></Dialog>;
}

function ConfigurationDialog({ form, setForm, processors, tenants, onSave, saving }: { form: ConfigurationForm | null; setForm: (form: ConfigurationForm | null) => void; processors: InsightProcessor[]; tenants: Array<{ id: string; name: string }>; onSave: () => void; saving: boolean }) {
  if (!form) return null;
  const scopeValid = form.scope === "global" || (form.tenant_id && (form.scope !== "project" || form.project_id));
  return <Dialog open onOpenChange={(open) => !open && setForm(null)}><DialogContent className="sm:max-w-2xl"><DialogHeader><DialogTitle>{form.id ? "Edit" : "Create"} insight configuration</DialogTitle><DialogDescription>Matching configurations all run; none is treated as the single built-in distiller.</DialogDescription></DialogHeader><div className="max-h-[70vh] space-y-4 overflow-y-auto py-2 pr-1">
    <div className="grid grid-cols-2 gap-4"><Field label="Name"><Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field><Field label="Service"><Select value={form.processor_id} onValueChange={(value) => setForm({ ...form, processor_id: value })}><SelectTrigger><SelectValue placeholder="Select a service" /></SelectTrigger><SelectContent>{processors.map((processor) => <SelectItem key={processor.id} value={processor.id}>{processor.name}{processor.status === "disabled" ? " (disabled)" : ""}</SelectItem>)}</SelectContent></Select></Field></div>
    <div className="grid grid-cols-2 gap-4"><Field label="Scope"><Select value={form.scope} onValueChange={(value: ConfigurationForm["scope"]) => setForm({ ...form, scope: value, tenant_id: value === "global" ? "" : form.tenant_id, project_id: value === "project" ? form.project_id : "" })}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent><SelectItem value="global">All tenants and projects</SelectItem><SelectItem value="tenant">One tenant</SelectItem><SelectItem value="project">One project</SelectItem></SelectContent></Select></Field>{form.scope !== "global" && <Field label="Tenant"><Select value={form.tenant_id} onValueChange={(value) => setForm({ ...form, tenant_id: value })}><SelectTrigger><SelectValue placeholder="Select tenant" /></SelectTrigger><SelectContent>{tenants.map((tenant) => <SelectItem key={tenant.id} value={tenant.id}>{tenant.name}</SelectItem>)}</SelectContent></Select></Field>}</div>
    {form.scope === "project" && <Field label="Project ID"><Input value={form.project_id} onChange={(event) => setForm({ ...form, project_id: event.target.value })} placeholder="mprj_…" /></Field>}
    <Field label="Instructions"><textarea className="min-h-28 w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring" value={form.instructions} onChange={(event) => setForm({ ...form, instructions: event.target.value })} placeholder="What should this configuration look for?" /></Field>
    <div className="grid gap-3 rounded-md border p-4 sm:grid-cols-3"><Toggle label="Allow new tags" description="Model may propose additions." checked={form.allow_new_tags} onChange={(checked) => setForm({ ...form, allow_new_tags: checked })} /><Toggle label="Session summary" description="Optional one or two lines." checked={form.include_session_summary} onChange={(checked) => setForm({ ...form, include_session_summary: checked })} /><Toggle label="Insight summaries" description="Optional one or two lines each." checked={form.include_insight_summaries} onChange={(checked) => setForm({ ...form, include_insight_summaries: checked })} /></div>
    <div className="grid grid-cols-3 gap-4"><Field label="Max transcript items"><Input type="number" min={1} value={form.max_input_items} onChange={(event) => setForm({ ...form, max_input_items: Number(event.target.value) })} /></Field><Field label="Max transcript characters"><Input type="number" min={1000} value={form.max_input_chars} onChange={(event) => setForm({ ...form, max_input_chars: Number(event.target.value) })} /></Field><Field label="Status"><Select value={form.status} onValueChange={(value: "active" | "disabled") => setForm({ ...form, status: value })}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent><SelectItem value="active">Active</SelectItem><SelectItem value="disabled">Disabled</SelectItem></SelectContent></Select></Field></div>
  </div><DialogFooter><Button variant="outline" onClick={() => setForm(null)}>Cancel</Button><Button onClick={onSave} disabled={saving || !form.name.trim() || !form.processor_id || !scopeValid}>Save configuration</Button></DialogFooter></DialogContent></Dialog>;
}

function TagDialog({ form, setForm, onSave, saving }: { form: { originalName?: string; name: string; description: string } | null; setForm: (form: { originalName?: string; name: string; description: string } | null) => void; onSave: () => void; saving: boolean }) {
  if (!form) return null;
  return <Dialog open onOpenChange={(open) => !open && setForm(null)}><DialogContent><DialogHeader><DialogTitle>{form.originalName ? "Edit" : "Add"} tag</DialogTitle><DialogDescription>Current tags and descriptions are supplied to every configured model.</DialogDescription></DialogHeader><div className="space-y-4 py-2"><Field label="Name"><Input value={form.name} disabled={Boolean(form.originalName)} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="architecture" /></Field><Field label="Description"><Input value={form.description} onChange={(event) => setForm({ ...form, description: event.target.value })} placeholder="When this tag should be used" /></Field></div><DialogFooter><Button variant="outline" onClick={() => setForm(null)}>Cancel</Button><Button onClick={onSave} disabled={saving || !form.name.trim()}>Save tag</Button></DialogFooter></DialogContent></Dialog>;
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <div className="space-y-2"><Label>{label}</Label>{children}</div>;
}

function Toggle({ label, description, checked, onChange }: { label: string; description: string; checked: boolean; onChange: (checked: boolean) => void }) {
  return <div className="flex items-start justify-between gap-3"><div><div className="text-sm font-medium">{label}</div><div className="text-xs text-muted-foreground">{description}</div></div><Switch checked={checked} onCheckedChange={onChange} /></div>;
}
