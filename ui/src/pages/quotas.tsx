import { useState, useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { tenants as tenantsApi } from "@/lib/api";
import { Gauge, ExternalLink, ChevronDown, ChevronRight, SlidersHorizontal } from "lucide-react";
import { PageContainer } from "@/components/page";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { SpendQuotaView } from "@/components/quota/spend-quota-view";
import { QuotaPoliciesPanel } from "@/components/quota/quota-policies-panel";

export function QuotasPage() {
  const { data: tenantsData } = useQuery({
    queryKey: ["tenants"],
    queryFn: () => tenantsApi.list(),
  });

  const tenantList = tenantsData?.tenants ?? [];
  const [tenantId, setTenantId] = useState<string>("");
  const [showAdvanced, setShowAdvanced] = useState(false);

  // Default to the first tenant once loaded.
  useEffect(() => {
    if (!tenantId && tenantList.length > 0) {
      const first = tenantList[0];
      if (first) setTenantId(first.id);
    }
  }, [tenantId, tenantList]);

  const selected = tenantList.find((t) => t.id === tenantId);

  return (
    <PageContainer>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-bold tracking-tight">
            <Gauge className="h-6 w-6 text-primary" />
            Spend &amp; Quotas
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Monthly spend budgets per tenant and user — assign, change, and watch spend in real time.
          </p>
        </div>

        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">Tenant</span>
          <Select value={tenantId} onValueChange={setTenantId}>
            <SelectTrigger className="w-56">
              <SelectValue placeholder="Select a tenant" />
            </SelectTrigger>
            <SelectContent>
              {tenantList.map((t) => (
                <SelectItem key={t.id} value={t.id}>{t.name}</SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      {tenantList.length === 0 ? (
        <div className="rounded-xl border border-border/70 bg-card p-10 text-center text-muted-foreground">
          No tenants yet. Create a tenant first to manage its quotas.
        </div>
      ) : tenantId ? (
        <>
          {selected && (
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <span className="font-medium text-foreground">{selected.name}</span>
              <Link
                to={`/tenants/${tenantId}`}
                className="inline-flex items-center gap-1 text-primary hover:underline"
              >
                Open <ExternalLink className="h-3 w-3" />
              </Link>
            </div>
          )}

          <SpendQuotaView tenantId={tenantId} />

          {/* Advanced: rate, token, and concurrency limits (not spend). */}
          <div className="border-t border-border/60 pt-4">
            <button
              onClick={() => setShowAdvanced((v) => !v)}
              className="flex items-center gap-2 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground"
            >
              {showAdvanced ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
              <SlidersHorizontal className="h-4 w-4" />
              Advanced limits (rate, tokens, concurrency)
            </button>
            {showAdvanced && (
              <div className="mt-4">
                <QuotaPoliciesPanel tenantId={tenantId} />
              </div>
            )}
          </div>
        </>
      ) : null}
    </PageContainer>
  );
}
