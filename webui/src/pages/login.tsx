import { useEffect, useState, type FormEvent } from "react";
import { Navigate, useSearchParams } from "react-router-dom";
import { LogIn } from "lucide-react";
import { api, ApiError, startLogin } from "@/lib/api";
import { safeLoginReturn } from "@/lib/login";
import type { AuthConfig } from "@/lib/types";
import { useSession } from "@/hooks/use-session";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

const ERROR_MESSAGES: Record<string, string> = {
  domain_not_allowed: "Your email domain is not set up for this console yet. Ask an administrator to add it.",
  account_inactive: "Your account is not active. Ask an administrator to restore it.",
  email_unverified: "Your identity provider has not verified your email address.",
  state_mismatch: "That sign-in attempt expired. Try again.",
  provider_denied: "Your identity provider declined the sign-in.",
  login_failed: "Sign-in did not complete. Try again.",
};

export function LoginPage() {
  const { me, loading, refresh } = useSession();
  const [params] = useSearchParams();
  const [config, setConfig] = useState<AuthConfig | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  // The key lives only in this input until the server sets its HttpOnly cookie.
  // Never write it to localStorage/sessionStorage or put it in a URL.
  const [key, setKey] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [keyError, setKeyError] = useState("");
  const returnTo = safeLoginReturn(params.get("return"));

  useEffect(() => {
    let cancelled = false;
    api.authConfig().then((cfg) => {
      if (!cancelled) setConfig(cfg);
    }).catch(() => {
      if (!cancelled) setUnavailable(true);
    });
    return () => { cancelled = true; };
  }, []);

  async function signInWithKey(event: FormEvent) {
    event.preventDefault();
    setKeyError("");
    setSubmitting(true);
    try {
      await api.loginWithKey(key.trim());
      setKey("");
      // /me supplies the CSRF token; me triggers the redirect.
      if (!await refresh()) setKeyError("Signed in, but your account could not be loaded. Refresh the page to retry.");
    } catch (error) {
      setKeyError(error instanceof ApiError && error.status === 401
        ? "Use a valid personal proxy key linked to an active user."
        : "Could not sign in. Check the portal connection and try again.");
    } finally {
      setSubmitting(false);
    }
  }

  if (!loading && me) return <Navigate to={returnTo} replace />;
  const errorCode = params.get("error");
  const error = keyError || (errorCode ? (ERROR_MESSAGES[errorCode] ?? ERROR_MESSAGES.login_failed) : null);

  return (
    <div className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm border-border shadow-2xl shadow-black/40">
        <CardHeader className="items-center pb-2 pt-8">
          <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-lg bg-primary/12 ring-1 ring-inset ring-primary/30">
            <span className="grid grid-cols-2 gap-1" aria-hidden>
              <i className="h-2 w-2 rounded-[2px] bg-primary" />
              <i className="h-2 w-2 rounded-[2px] bg-primary/40" />
              <i className="h-2 w-2 rounded-[2px] bg-primary/40" />
              <i className="h-2 w-2 rounded-[2px] bg-primary" />
            </span>
          </div>
          <CardTitle className="font-display text-xl">Pi Console</CardTitle>
          <CardDescription>Sign in with your existing proxy key</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {error && <div role="alert" className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</div>}
          {unavailable ? (
            <div className="rounded-md bg-muted px-3 py-2 text-sm text-muted-foreground">The console is not reachable right now. Try again shortly.</div>
          ) : !config ? (
            <p className="text-sm text-muted-foreground">Checking sign-in options…</p>
          ) : (
            <>
              {config.api_key_enabled && (
                <form onSubmit={signInWithKey} className="space-y-4">
                  <div className="space-y-2">
                    <Label htmlFor="proxy-key">Proxy API key</Label>
                    <Input id="proxy-key" type="password" value={key} onChange={(event) => setKey(event.target.value)}
                      placeholder="cpk_live_…" autoComplete="off" autoCapitalize="none" spellCheck={false}
                      disabled={submitting} autoFocus required />
                    <p className="text-xs text-muted-foreground">Stored in an encrypted, secure cookie. Your existing permissions and quotas apply.</p>
                  </div>
                  <Button type="submit" className="w-full" disabled={submitting || !key.trim()}>
                    {submitting ? "Signing in…" : "Sign in"}
                  </Button>
                </form>
              )}
              {config.oidc_enabled && (
                <Button className="w-full" variant={config.api_key_enabled ? "outline" : "default"} disabled={submitting}
                  onClick={() => startLogin(returnTo)}>
                  <LogIn className="mr-2 h-4 w-4" />Continue with {config.provider_name || "your work account"}
                </Button>
              )}
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
