import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Line, LineChart, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis, CartesianGrid, Legend } from "recharts";
import { providers } from "@/lib/api";
import { usageWindowLabel } from "@/lib/provider-balance";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { ProviderBalanceWindow } from "@/lib/types";

const colors = ["#2563eb", "#7c3aed", "#0891b2", "#db2777"];
const windowKey = (window: ProviderBalanceWindow) => `${window.name}:${window.window_seconds ?? 0}`;
const dateLabel = (value: number | string) => new Date(value).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });

export function ProviderBalanceHistory({ accountId }: { accountId: string }) {
  const [period, setPeriod] = useState("7");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [custom, setCustom] = useState<{ from: string; to: string }>();
  const [rangeError, setRangeError] = useState("");
  const { data, isPending, isError, refetch, isFetching } = useQuery({
    queryKey: ["provider-balance-history", accountId, period, custom],
    queryFn: () => {
      const now = Date.now();
      const range = period === "custom" && custom ? custom : {
        from: new Date(now - Number(period === "custom" ? 7 : period) * 86400000).toISOString(),
        to: new Date(now).toISOString(),
      };
      return providers.balanceHistory(accountId, 10000, range);
    },
    refetchInterval: 60_000,
  });
  const windows = new Map<string, ProviderBalanceWindow>();
  for (const snapshot of data?.provider_balances ?? []) {
    if (snapshot.status === "success" && !snapshot.normalization_error) {
      for (const window of snapshot.usage_windows ?? []) windows.set(windowKey(window), window);
    }
  }
  const series = [...windows.entries()].map(([key, window], index) => ({ key, window, dataKey: `window${index}` }));
  const points = (data?.provider_balances ?? []).map((snapshot) => {
    const point: Record<string, number | null> = { at: new Date(snapshot.observed_at).getTime() };
    for (const item of series) {
      const window = snapshot.usage_windows?.find((window) => windowKey(window) === item.key);
      point[item.dataKey] = snapshot.status === "success" && !snapshot.normalization_error && window ? window.used_percent : null;
    }
    point.balance = snapshot.status === "success" && !snapshot.normalization_error ? snapshot.amount_remaining ?? null : null;
    return point;
  });
  const resets = data?.reset_events ?? [];
  const applyRange = () => {
    const start = new Date(from), end = new Date(to);
    if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || start >= end) {
      setRangeError("Choose a start time before the end time.");
      return;
    }
    setCustom({ from: start.toISOString(), to: end.toISOString() });
    setRangeError("");
  };
  return (
    <Card>
      <CardHeader className="gap-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle>Upstream usage history</CardTitle>
          <div className="flex gap-2">
            <Select value={period} onValueChange={setPeriod}>
              <SelectTrigger className="w-40" aria-label="History range"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="1">Last 24 hours</SelectItem>
                <SelectItem value="7">Last 7 days</SelectItem>
                <SelectItem value="30">Last 30 days</SelectItem>
                <SelectItem value="custom">Custom range</SelectItem>
              </SelectContent>
            </Select>
            <Button variant="outline" size="sm" disabled={isFetching} onClick={() => refetch()}>Refresh history</Button>
          </div>
        </div>
        <p className="text-sm text-muted-foreground">Provider-reported usage, including direct traffic. Dashed markers show allowance resets detected between checks. Times are local.</p>
        {period === "custom" && (
          <div className="flex flex-wrap items-end gap-3">
            <label className="text-xs">From<Input aria-label="History from" type="datetime-local" value={from} onChange={(e) => setFrom(e.target.value)} /></label>
            <label className="text-xs">To<Input aria-label="History to" type="datetime-local" value={to} onChange={(e) => setTo(e.target.value)} /></label>
            <Button size="sm" variant="outline" onClick={applyRange}>Apply range</Button>
            {rangeError && <p role="alert" className="text-sm text-destructive">{rangeError}</p>}
          </div>
        )}
      </CardHeader>
      <CardContent className="space-y-4">
        {isError ? <p role="alert" className="text-destructive">Could not load usage history. Try refreshing.</p> : isPending ? <p className="text-muted-foreground">Loading history…</p> : points.length === 0 ? <p className="text-muted-foreground">No observations in this range.</p> : (
          <>
            {data?.truncated && <p role="status" className="text-sm text-amber-700">Showing the latest 10,000 observations. Choose a shorter range to see all observations.</p>}
            <div className="h-80 w-full" role="img" aria-label={`Provider usage chart with ${resets.length} detected resets`}>
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={points} margin={{ top: 24, right: 24, bottom: 12, left: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" opacity={0.25} />
                  <XAxis dataKey="at" type="number" domain={["dataMin", "dataMax"]} tickFormatter={dateLabel} minTickGap={60} tick={{ fontSize: 11 }} />
                  <YAxis domain={series.length ? [0, 100] : ["auto", "auto"]} tickFormatter={(value) => series.length ? `${value}%` : String(value)} tick={{ fontSize: 11 }} />
                  <Tooltip labelFormatter={(value) => new Date(Number(value)).toLocaleString()} formatter={(value: number, name: string) => [series.length ? `${value.toFixed(1)}% used` : value.toLocaleString(), name]} contentStyle={{ backgroundColor: "var(--card)", borderColor: "var(--border)", borderRadius: 8 }} />
                  <Legend />
                  {series.map((item, index) => <Line key={item.key} type="stepAfter" dataKey={item.dataKey} name={`${usageWindowLabel(item.window)} (${item.window.name})`} stroke={colors[index % colors.length]} strokeWidth={2} dot={false} connectNulls={false} isAnimationActive={false} />)}
                  {!series.length && <Line dataKey="balance" name="Remaining balance" stroke={colors[0]} dot={false} connectNulls={false} isAnimationActive={false} />}
                  {resets.map((reset, index) => <ReferenceLine key={`${reset.window_name}-${reset.observed_at}`} x={new Date(reset.occurred_at).getTime()} stroke="#d97706" strokeDasharray="5 4" ifOverflow="extendDomain" label={{ value: `${reset.kind === "early" ? "Early reset" : "Reset"} ${index + 1}`, position: "insideTopLeft", fill: "#b45309", fontSize: 12 }} />)}
                </LineChart>
              </ResponsiveContainer>
            </div>
            <p className="text-xs text-muted-foreground">{points.length.toLocaleString()} observations · {resets.length} detected {resets.length === 1 ? "reset" : "resets"}. Missing or failed checks appear as gaps.</p>
            {resets.length > 0 ? (
              <Table>
                <TableHeader><TableRow><TableHead>Reset</TableHead><TableHead>Window</TableHead><TableHead>When</TableHead><TableHead>Usage before → after</TableHead><TableHead>Next reset</TableHead></TableRow></TableHeader>
                <TableBody>{resets.map((reset, index) => (
                  <TableRow key={`${reset.window_name}-${reset.observed_at}`}>
                    <TableCell className="font-medium">{index + 1}. {reset.kind === "early" ? "Early reset detected" : "Scheduled reset"}</TableCell>
                    <TableCell>{usageWindowLabel({ name: reset.window_name, window_seconds: reset.window_seconds, used_percent: 0, remaining_percent: 0 })}</TableCell>
                    <TableCell><p>{dateLabel(reset.occurred_at)}</p>{reset.kind === "early" && <p className="text-xs text-muted-foreground">Observed between {dateLabel(reset.previous_observed_at)} and {dateLabel(reset.observed_at)}</p>}</TableCell>
                    <TableCell>{reset.previous_used_percent}% → {reset.used_percent}%</TableCell>
                    <TableCell>{reset.resets_at ? dateLabel(reset.resets_at) : "—"}</TableCell>
                  </TableRow>
                ))}</TableBody>
              </Table>
            ) : <p className="text-sm text-muted-foreground">No resets detected in this range.</p>}
          </>
        )}
      </CardContent>
    </Card>
  );
}
