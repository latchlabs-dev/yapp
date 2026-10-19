import { ProviderBalanceHistory } from "@/components/provider-balance-history";
import { useState } from "react";
import { useParams, Link } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { providers as providersApi, credentials as credsApi } from "@/lib/api";
import { ArrowLeft, Plus, Trash2, KeyRound, CreditCard, Pencil, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { PageContainer } from "@/components/page";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import { formatDateFull, timeAgo } from "@/lib/utils";
import { formatProviderBalance, providerBillingLabel, usageWindowLabel } from "@/lib/provider-balance";
import { toast } from "sonner";

type CredentialMode = "api_key" | "oauth";
type BillingMode = "default" | "subscription" | "metered";

function billingFormFromConfig(config?: Record<string, unknown>) {
  const billing = (config?.billing ?? {}) as Record<string, unknown>;
  return {
    mode: (billing.mode as BillingMode) || "default",
    currency: String(billing.currency || "USD"),
    monthlyCost: String(Number(billing.monthly_cost_cents || 0) / 100),
    fallbackRate: String(billing.fallback_usd_per_million_tokens || 0),
    inputPrice: String(billing.input_price_per_million || 0),
    outputPrice: String(billing.output_price_per_million || 0),
    cacheReadPrice: String(billing.cache_read_price_per_million || 0),
    cacheWritePrice: String(billing.cache_write_price_per_million || 0),
    fiveHourQuota: String(billing.rolling_five_hour_quota || 0),
    sevenDayQuota: String(billing.rolling_seven_day_quota || 0),
  };
}

function emptyCredentialForm(mode: CredentialMode = "api_key") {
  return {
    mode,
    apiKey: "",
    accessToken: "",
    refreshToken: "",
    accountID: "",
    oauthTokenURL: "",
    oauthClientID: "",
    oauthScope: "",
    expiresAt: "",
  };
}

function isoDateTime(value: string): string | undefined {
  if (!value.trim()) return undefined;
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value.trim() : parsed.toISOString();
}

export function ProviderDetailPage() {
  const { accountId } = useParams<{ accountId: string }>();
  const qc = useQueryClient();

  const { data: account } = useQuery({
    queryKey: ["provider-accounts", accountId],
    queryFn: () => providersApi.get(accountId!),
    enabled: !!accountId,
    refetchInterval: 60_000,
  });

  const { data: creds } = useQuery({
    queryKey: ["credentials", accountId],
    queryFn: () => credsApi.list(accountId!),
    enabled: !!accountId,
  });

  const { data: rateCardData } = useQuery({
    queryKey: ["subscription-rate-card", accountId],
    queryFn: () => providersApi.subscriptionRateCard(accountId!),
    enabled: !!accountId,
    refetchInterval: 60_000,
  });

  const [showBilling, setShowBilling] = useState(false);
	const [paidPeriod, setPaidPeriod] = useState({ start: "", end: "", amount: "200" });
	const { data: periodData, isError: periodError } = useQuery({
		queryKey: ["subscription-billing-periods", accountId],
		queryFn: () => providersApi.billingPeriods(accountId!),
		enabled: !!accountId,
		refetchInterval: 60_000,
	});
	const createPeriodMut = useMutation({
		mutationFn: () => providersApi.createBillingPeriod(accountId!, {
			starts_at: new Date(paidPeriod.start).toISOString(), ends_at: new Date(paidPeriod.end).toISOString(),
			amount_millicents: Math.round(Number(paidPeriod.amount) * 100_000),
		}),
		onSuccess: () => {
			qc.invalidateQueries({ queryKey: ["subscription-billing-periods", accountId] });
			qc.invalidateQueries({ queryKey: ["provider-accounts", accountId] });
			setPaidPeriod({ ...paidPeriod, start: "", end: "" });
			toast.success("Paid billing period recorded");
		},
		onError: (err) => toast.error("Could not record billing period", { description: err.message }),
	});
  const [billingForm, setBillingForm] = useState(() => billingFormFromConfig());
  const billing = (account?.config?.billing ?? {}) as Record<string, unknown>;

  const openBilling = () => {
    setBillingForm(billingFormFromConfig(account?.config));
    setShowBilling(true);
  };

  const updateBillingMut = useMutation({
    mutationFn: () =>
      providersApi.update(accountId!, {
        config: {
          ...account?.config,
          billing: {
			...billing,
            mode: billingForm.mode,
            currency: billingForm.currency.trim() || "USD",
            monthly_cost_cents: Math.round(Number(billingForm.monthlyCost || 0) * 100),
            fallback_usd_per_million_tokens: Number(billingForm.fallbackRate || 0),
            input_price_per_million: Number(billingForm.inputPrice || 0),
            output_price_per_million: Number(billingForm.outputPrice || 0),
            cache_read_price_per_million: Number(billingForm.cacheReadPrice || 0),
            cache_write_price_per_million: Number(billingForm.cacheWritePrice || 0),
            rolling_five_hour_quota: Number(billingForm.fiveHourQuota || 0),
            rolling_seven_day_quota: Number(billingForm.sevenDayQuota || 0),
          },
        },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["provider-accounts", accountId] });
      qc.invalidateQueries({ queryKey: ["provider-accounts"] });
      qc.invalidateQueries({ queryKey: ["provider-balance-history", accountId] });
      qc.invalidateQueries({ queryKey: ["subscription-rate-card", accountId] });
      setShowBilling(false);
      toast.success("Billing configuration updated");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  const refreshBalanceMut = useMutation({
    mutationFn: () => providersApi.refreshBalance(accountId!),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["provider-accounts", accountId] });
      qc.invalidateQueries({ queryKey: ["provider-accounts"] });
      qc.invalidateQueries({ queryKey: ["provider-balance-history", accountId] });
      toast.success("Provider balance checked");
    },
    onError: (err) => toast.error("Balance check failed", { description: err.message }),
  });

  // Create credential
  const [showAddCred, setShowAddCred] = useState(false);
  const [credForm, setCredForm] = useState(() => emptyCredentialForm());

  const openAddCredential = () => {
    setCredForm(emptyCredentialForm(account?.provider === "openai-codex-responses" ? "oauth" : "api_key"));
    setShowAddCred(true);
  };

  const createCredMut = useMutation({
    mutationFn: () => {
      const payload =
        credForm.mode === "oauth"
          ? {
              access_token: credForm.accessToken.trim(),
              refresh_token: credForm.refreshToken.trim(),
              account_id: credForm.accountID.trim() || undefined,
              oauth_token_url: credForm.oauthTokenURL.trim() || undefined,
              oauth_client_id: credForm.oauthClientID.trim() || undefined,
              oauth_scope: credForm.oauthScope.trim() || undefined,
              expires_at: isoDateTime(credForm.expiresAt),
            }
          : { api_key: credForm.apiKey.trim() };
      return credsApi.create(accountId!, payload);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["credentials", accountId] });
      setShowAddCred(false);
      setCredForm(emptyCredentialForm());
      toast.success("Credential added");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  const revokeCredMut = useMutation({
    mutationFn: (credentialId: string) => credsApi.revoke(accountId!, credentialId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["credentials", accountId] });
      toast.success("Credential revoked");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  return (
    <PageContainer>
      <div className="flex items-center gap-3">
        <Link to="/providers" className="text-muted-foreground hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          <span className="sr-only">Back to providers</span>
        </Link>
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{account?.name || "Provider"}</h1>
          {account?.id && <p className="text-xs font-mono text-muted-foreground">{account.id}</p>}
        </div>
        <div className="ml-auto flex gap-2">
          <Badge variant="info">{account?.provider}</Badge>
          <Badge variant={account?.status === "active" ? "success" : "default"}>{account?.status}</Badge>
        </div>
      </div>

      {/* Credentials */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
            <KeyRound className="h-4 w-4" />
            Credentials
          </CardTitle>
          <Button size="sm" onClick={openAddCredential}>
            <Plus className="h-4 w-4" /> Add Credential
          </Button>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Version</TableHead>
                <TableHead>Fingerprint</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Created By</TableHead>
                <TableHead>Last Used</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="w-12"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {creds?.credentials?.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={7} className="text-center py-8 text-muted-foreground">
                    No credentials configured
                  </TableCell>
                </TableRow>
              ) : (
                creds?.credentials?.map((c) => (
                  <TableRow key={c.id} className="data-row">
                    <TableCell className="font-mono">v{c.version}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">{c.fingerprint}</TableCell>
                    <TableCell>
                      <Badge variant={
                        c.status === "active" ? "success" :
                        c.status === "revoked" ? "destructive" :
                        c.status === "pending_validation" ? "warning" : "default"
                      }>
                        {c.status}
                      </Badge>
                    </TableCell>
                    <TableCell>{c.created_by}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {c.last_used_at ? timeAgo(c.last_used_at) : "—"}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDateFull(c.created_at)}</TableCell>
                    <TableCell>
                      {c.status === "active" && (
                        <Button variant="ghost" size="icon" aria-label="Revoke credential" onClick={() => revokeCredMut.mutate(c.id)}>
                          <Trash2 className="h-3.5 w-3.5 text-muted-foreground hover:text-destructive" />
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* Billing */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
            <CreditCard className="h-4 w-4" />
            Billing
          </CardTitle>
          <div className="flex gap-2">
            <Button size="sm" variant="outline" onClick={() => refreshBalanceMut.mutate()} disabled={refreshBalanceMut.isPending}>
              <RefreshCw className={`h-4 w-4 ${refreshBalanceMut.isPending ? "animate-spin" : ""}`} />
              Check now
            </Button>
            <Button size="sm" variant="outline" onClick={openBilling}>
              <Pencil className="h-4 w-4" /> Edit
            </Button>
          </div>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-5">
          <div>
            <p className="text-xs text-muted-foreground">Billing type</p>
            <p className="font-medium">{providerBillingLabel(account)}</p>
          </div>
          <div>
            <p className="text-xs text-muted-foreground">Available</p>
            <p className={account?.balance?.status === "error" ? "font-medium text-destructive" : "font-medium"}>
              {formatProviderBalance(account?.balance)}
            </p>
          </div>
          <div>
            <p className="text-xs text-muted-foreground">Last checked</p>
            <p className="font-medium" title={account?.balance?.observed_at ? formatDateFull(account.balance.observed_at) : undefined}>
              {account?.balance?.observed_at ? timeAgo(account.balance.observed_at) : "—"}
            </p>
          </div>
          <div>
            <p className="text-xs text-muted-foreground">Monthly fixed cost</p>
            <p className="font-medium">
              {String(billing.currency || "USD")} {(Number(billing.monthly_cost_cents || 0) / 100).toFixed(2)}
            </p>
          </div>
          <div>
            <p className="text-xs text-muted-foreground">Refresh status</p>
            <p className="font-medium capitalize">{account?.balance?.status || "pending"}</p>
          </div>
        </CardContent>
        {(account?.balance?.usage_windows?.length ?? 0) > 0 && (
          <CardContent className="border-t pt-4 grid gap-3 md:grid-cols-3">
            {account!.balance!.usage_windows!.map((window) => (
              <div key={`${window.name}-${window.window_seconds || 0}`} className="rounded-md bg-muted/50 px-3 py-2">
                <p className="text-xs text-muted-foreground capitalize">{usageWindowLabel(window)} allowance</p>
                <p className="font-medium">
                  {typeof window.remaining === "number" && typeof window.limit !== "number"
                    ? `${window.remaining.toLocaleString()} ${account!.balance!.unit || "units"} remaining`
                    : `${window.remaining_percent.toFixed(0)}% remaining`}
                </p>
                {window.resets_at && <p className="text-xs text-muted-foreground">Resets {timeAgo(window.resets_at)}</p>}
              </div>
            ))}
          </CardContent>
        )}
		{billing.mode === "subscription" && (
			<CardContent className="border-t pt-4 space-y-4">
				<div><p className="text-sm font-medium">Paid subscription periods</p>
				<p className="text-xs text-muted-foreground">Cost follows observed allowance consumption, capped at the amount paid for each period. Allowance resets do not renew this budget. Requests await the next allowance observation; unmatched cost remains unattributed.</p></div>
				{periodError && <p className="text-sm text-destructive">Could not load billing periods.</p>}
				{!periodError && !periodData?.billing_periods.length && <p className="text-sm text-muted-foreground">Record verified paid dates to enable capped allocation.</p>}
				{periodData?.billing_periods.map((p) => (
					<div key={p.id} className="rounded-md bg-muted/50 p-3 space-y-2">
						<p className="text-sm font-medium">{formatDateFull(p.starts_at)} – {formatDateFull(p.ends_at)}</p>
						<div className="grid gap-3 md:grid-cols-4 text-sm">
							<div>Paid amount<p className="font-mono">${(p.amount_millicents / 100_000).toFixed(2)}</p></div>
							<div>Allocated (includes unattributed)<p className="font-mono">${(p.allocated_millicents / 100_000).toFixed(2)}</p></div>
							<div>Unattributed<p className="font-mono">${(p.unattributed_millicents / 100_000).toFixed(2)}</p></div>
							<div>Remaining budget<p className="font-mono">${((p.amount_millicents - p.allocated_millicents) / 100_000).toFixed(2)}</p></div>
						</div>
						<p className="text-xs text-muted-foreground">{p.consumed_points.toFixed(2)} allowance points consumed · {p.observed_through ? `Observed through ${formatDateFull(p.observed_through)}` : "Awaiting allowance observations"}</p>
					</div>
				))}
				<details><summary className="text-sm cursor-pointer">Record a paid renewal</summary>
					<div className="grid gap-3 md:grid-cols-3 mt-3">
						<label className="text-sm">Paid period start (local time)<Input type="datetime-local" value={paidPeriod.start} onChange={(e) => setPaidPeriod({ ...paidPeriod, start: e.target.value })} /></label>
						<label className="text-sm">Paid period end (local time)<Input type="datetime-local" value={paidPeriod.end} onChange={(e) => setPaidPeriod({ ...paidPeriod, end: e.target.value })} /></label>
						<label className="text-sm">Amount paid (USD)<Input type="number" min="0.01" step="0.01" value={paidPeriod.amount} onChange={(e) => setPaidPeriod({ ...paidPeriod, amount: e.target.value })} /></label>
					</div>
					<Button className="mt-3" onClick={() => createPeriodMut.mutate()} disabled={createPeriodMut.isPending || !paidPeriod.start || !paidPeriod.end || !(Number(paidPeriod.amount) > 0)}>Record paid period</Button>
				</details>
			</CardContent>
		)}
        {billing.allocation_method !== "observed_delta" && (rateCardData?.rate_cards?.length ?? 0) > 0 && (
          <CardContent className="border-t pt-4">
            <div className="mb-3">
              <p className="text-sm font-medium">Active quota rate card</p>
              <p className="text-xs text-muted-foreground">
                Allocates provider-wide subscription usage, including direct traffic, across proxied tokens. Completed requests are charged before the next quota check.
              </p>
            </div>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Model</TableHead>
                  <TableHead className="text-right">Source tokens</TableHead>
                  <TableHead className="text-right">Provider use</TableHead>
                  <TableHead className="text-right">Allocated USD</TableHead>
                  <TableHead className="text-right">USD / 1M tokens</TableHead>
                  <TableHead>Basis</TableHead>
                  <TableHead>Calculated</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rateCardData!.rate_cards.map((card) => (
                  <TableRow key={card.id}>
                    <TableCell className="font-mono text-xs">{card.model === "*" ? "All models fallback" : card.model}</TableCell>
                    <TableCell className="text-right font-mono">{card.source_tokens.toLocaleString()}</TableCell>
                    <TableCell className="text-right font-mono">
                      {card.source_usage_percent.toFixed(2)}%
                      {card.source_usage_window_seconds
                        ? ` / ${usageWindowLabel({
                            name: card.source_usage_window || "allowance",
                            used_percent: 0,
                            remaining_percent: 0,
                            window_seconds: card.source_usage_window_seconds,
                          })}`
                        : ""}
                    </TableCell>
                    <TableCell className="text-right font-mono">${(card.accrued_cost_cents / 100).toFixed(2)}</TableCell>
                    <TableCell className="text-right font-mono">${card.usd_per_million_tokens.toFixed(6)}</TableCell>
                    <TableCell><Badge variant="default">{card.calculation_method.replaceAll("_", " ")}</Badge></TableCell>
                    <TableCell className="text-xs text-muted-foreground">{timeAgo(card.calculated_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        )}
        {account?.balance?.status === "error" && account.balance.error_message && (
          <CardContent className="border-t pt-4 text-sm text-destructive">
            {account.balance.error_message}
          </CardContent>
        )}
      </Card>

      {accountId && <ProviderBalanceHistory accountId={accountId} />}

      {/* Config */}
      {account?.config && Object.keys(account.config).length > 0 && (
        <Card>
          <CardHeader><CardTitle className="text-sm font-medium text-muted-foreground">Configuration</CardTitle></CardHeader>
          <CardContent>
            <pre className="rounded-md bg-muted p-4 overflow-auto text-xs font-mono">
              {JSON.stringify(account.config, null, 2)}
            </pre>
          </CardContent>
        </Card>
      )}

      <Dialog open={showAddCred} onOpenChange={setShowAddCred}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add Credential</DialogTitle>
            <DialogDescription>Store an upstream API key or OAuth token pair securely.</DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Type</label>
              <Select value={credForm.mode} onValueChange={(mode) => setCredForm(emptyCredentialForm(mode as CredentialMode))}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="api_key">API Key</SelectItem>
                  <SelectItem value="oauth">OAuth Tokens</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {credForm.mode === "oauth" ? (
              <>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Access Token</label>
                  <Input
                    type="password"
                    value={credForm.accessToken}
                    onChange={(e) => setCredForm({ ...credForm, accessToken: e.target.value })}
                    className="font-mono"
                  />
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Refresh Token</label>
                  <Input
                    type="password"
                    value={credForm.refreshToken}
                    onChange={(e) => setCredForm({ ...credForm, refreshToken: e.target.value })}
                    className="font-mono"
                  />
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Account ID</label>
                    <Input
                      value={credForm.accountID}
                      onChange={(e) => setCredForm({ ...credForm, accountID: e.target.value })}
                      className="font-mono"
                    />
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Expires At</label>
                    <Input
                      type="datetime-local"
                      value={credForm.expiresAt}
                      onChange={(e) => setCredForm({ ...credForm, expiresAt: e.target.value })}
                    />
                  </div>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                  <div className="space-y-2">
                    <label className="text-sm font-medium">OAuth Token URL</label>
                    <Input
                      value={credForm.oauthTokenURL}
                      onChange={(e) => setCredForm({ ...credForm, oauthTokenURL: e.target.value })}
                      className="font-mono"
                      placeholder="Uses provider config when blank"
                    />
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">OAuth Client ID</label>
                    <Input
                      value={credForm.oauthClientID}
                      onChange={(e) => setCredForm({ ...credForm, oauthClientID: e.target.value })}
                      className="font-mono"
                      placeholder="Uses provider config when blank"
                    />
                  </div>
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">OAuth Scope</label>
                  <Input
                    value={credForm.oauthScope}
                    onChange={(e) => setCredForm({ ...credForm, oauthScope: e.target.value })}
                    className="font-mono"
                  />
                </div>
              </>
            ) : (
              <div className="space-y-2">
                <label className="text-sm font-medium">API Key</label>
                <Input
                  type="password"
                  placeholder="sk-..."
                  value={credForm.apiKey}
                  onChange={(e) => setCredForm({ ...credForm, apiKey: e.target.value })}
                  className="font-mono"
                />
              </div>
            )}
          </div>
          <DialogFooter>
            <DialogClose asChild><Button variant="outline">Cancel</Button></DialogClose>
            <Button
              onClick={() => createCredMut.mutate()}
              disabled={
                createCredMut.isPending ||
                (credForm.mode === "oauth"
                  ? !credForm.refreshToken.trim() && !credForm.accessToken.trim()
                  : !credForm.apiKey.trim())
              }
            >
              {createCredMut.isPending ? "Adding..." : "Add Credential"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={showBilling} onOpenChange={setShowBilling}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Billing Configuration</DialogTitle>
            <DialogDescription>
              Choose how usage costs are calculated for this provider account. Record paid subscription periods to allocate observed allowance consumption with a fixed cap.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Billing mode</label>
              <Select value={billingForm.mode} onValueChange={(mode) => setBillingForm({ ...billingForm, mode: mode as BillingMode })}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="default">Pay as you go — model pricing</SelectItem>
                  <SelectItem value="subscription">Subscription</SelectItem>
                  <SelectItem value="metered">Custom per-token pricing</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-2">
                <label className="text-sm font-medium">Currency</label>
                <Input value={billingForm.currency} onChange={(e) => setBillingForm({ ...billingForm, currency: e.target.value })} />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Monthly fixed cost</label>
                <Input type="number" min="0" step="0.01" value={billingForm.monthlyCost} onChange={(e) => setBillingForm({ ...billingForm, monthlyCost: e.target.value })} />
              </div>
            </div>
            {billingForm.mode === "subscription" && billing.allocation_method !== "observed_delta" && (
              <div className="space-y-2">
                <label className="text-sm font-medium">Bootstrap fallback USD / 1M tokens</label>
                <Input type="number" min="0" step="0.000001" value={billingForm.fallbackRate} onChange={(e) => setBillingForm({ ...billingForm, fallbackRate: e.target.value })} />
                <p className="text-xs text-muted-foreground">Used only until cycle usage or a previous daily rate is available.</p>
              </div>
            )}
            {billingForm.mode === "metered" && (
              <div className="grid grid-cols-2 gap-3">
                {([
                  ["inputPrice", "Input / 1M tokens"],
                  ["outputPrice", "Output / 1M tokens"],
                  ["cacheReadPrice", "Cache read / 1M"],
                  ["cacheWritePrice", "Cache write / 1M"],
                ] as const).map(([field, label]) => (
                  <div className="space-y-2" key={field}>
                    <label className="text-sm font-medium">{label}</label>
                    <Input type="number" min="0" step="0.000001" value={billingForm[field]} onChange={(e) => setBillingForm({ ...billingForm, [field]: e.target.value })} />
                  </div>
                ))}
              </div>
            )}
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-2">
                <label className="text-sm font-medium">Rolling 5-hour quota</label>
                <Input type="number" min="0" step="1" value={billingForm.fiveHourQuota} onChange={(e) => setBillingForm({ ...billingForm, fiveHourQuota: e.target.value })} />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Rolling 7-day quota</label>
                <Input type="number" min="0" step="1" value={billingForm.sevenDayQuota} onChange={(e) => setBillingForm({ ...billingForm, sevenDayQuota: e.target.value })} />
              </div>
            </div>
          </div>
          <DialogFooter>
            <DialogClose asChild><Button variant="outline">Cancel</Button></DialogClose>
            <Button onClick={() => updateBillingMut.mutate()} disabled={updateBillingMut.isPending}>
              {updateBillingMut.isPending ? "Saving..." : "Save Billing"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </PageContainer>
  );
}
