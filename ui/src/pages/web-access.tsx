import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Globe, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { tenants, webAccess } from "@/lib/api";
import { PageContainer, PageHeading } from "@/components/page";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

/**
 * WebAccessPage controls who can sign into the public web console.
 *
 * A domain mapping is not an optimisation — it is the gate. `users` is unique
 * on (tenant_id, email), so an email address alone cannot identify a tenant,
 * and the login flow refuses an unmapped domain rather than guessing one. Until
 * a domain appears here, nobody can sign in at all.
 */
export function WebAccessPage() {
  const queryClient = useQueryClient();
  const [domain, setDomain] = useState("");
  const [tenantId, setTenantId] = useState("");

  const domains = useQuery({ queryKey: ["web-domains"], queryFn: () => webAccess.domains() });
  const identities = useQuery({ queryKey: ["web-identities"], queryFn: () => webAccess.identities() });
  const tenantList = useQuery({ queryKey: ["tenants"], queryFn: () => tenants.list() });

  const upsert = useMutation({
    mutationFn: () => webAccess.upsertDomain({ domain, tenant_id: tenantId }),
    onSuccess: () => {
      toast.success(`${domain} mapped`);
      setDomain("");
      void queryClient.invalidateQueries({ queryKey: ["web-domains"] });
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const remove = useMutation({
    mutationFn: (d: string) => webAccess.deleteDomain(d),
    onSuccess: (res) => {
      toast.success("Domain unmapped", { description: res.note });
      void queryClient.invalidateQueries({ queryKey: ["web-domains"] });
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const revoke = useMutation({
    mutationFn: (userId: string) => webAccess.revokeSessions(userId),
    onSuccess: () => toast.success("Browser sessions revoked"),
    onError: (err: Error) => toast.error(err.message),
  });

  return (
    <PageContainer>
      <PageHeading
        title="Web Console Access"
        description="Which email domains may sign into the public console, and who has."
      />
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Globe className="h-4 w-4" />
            Allowed domains
          </CardTitle>
          <CardDescription>
            A person signing in with an unmapped domain is refused, not placed in a guessed tenant.
            Sessions are granted the console scope set: model invocation, model listing, and project
            memory — never admin scopes.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-end gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="domain">Email domain</Label>
              <Input
                id="domain"
                value={domain}
                onChange={(e) => setDomain(e.target.value)}
                placeholder="example.com"
                className="w-56 font-mono text-sm"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="tenant">Tenant</Label>
              <Select value={tenantId} onValueChange={setTenantId}>
                <SelectTrigger id="tenant" className="w-64">
                  <SelectValue placeholder="Select a tenant" />
                </SelectTrigger>
                <SelectContent>
                  {(tenantList.data?.tenants ?? []).map((t) => (
                    <SelectItem key={t.id} value={t.id}>
                      {t.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button
              disabled={!domain.trim() || !tenantId || upsert.isPending}
              onClick={() => upsert.mutate()}
            >
              Map domain
            </Button>
          </div>

          {domains.isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : (domains.data?.domains ?? []).length === 0 ? (
            <p className="rounded-md bg-warning/10 px-3 py-2 text-sm text-warning">
              No domains are mapped, so nobody can sign into the web console yet.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Domain</TableHead>
                  <TableHead>Tenant</TableHead>
                  <TableHead>Scopes</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {domains.data!.domains!.map((d) => (
                  <TableRow key={d.domain}>
                    <TableCell className="font-mono text-sm">{d.domain}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {d.tenant_id}
                    </TableCell>
                    <TableCell className="font-mono text-[11px] text-muted-foreground">
                      {d.default_scopes.join(", ")}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => remove.mutate(d.domain)}
                        aria-label={`Unmap ${d.domain}`}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Signed-in identities</CardTitle>
          <CardDescription>
            Who has actually completed a sign-in. This is how you confirm a new domain mapping
            works. Revoking cuts a person's live browser sessions immediately; unmapping their
            domain only stops future sign-ins.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {identities.isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : (identities.data?.identities ?? []).length === 0 ? (
            <p className="py-4 text-sm text-muted-foreground">Nobody has signed in yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Email</TableHead>
                  <TableHead>Tenant</TableHead>
                  <TableHead>Last sign-in</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {identities.data!.identities!.map((identity) => (
                  <TableRow key={identity.id}>
                    <TableCell className="text-sm">{identity.email}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {identity.tenant_id}
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {identity.last_login_at
                        ? new Date(identity.last_login_at).toLocaleString()
                        : "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-xs"
                        onClick={() => revoke.mutate(identity.user_id)}
                      >
                        Revoke sessions
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </PageContainer>
  );
}
