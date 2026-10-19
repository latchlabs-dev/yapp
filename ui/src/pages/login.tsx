import { useState } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { Eye, EyeOff } from "lucide-react";
import { useAuth } from "@/hooks/use-auth";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

export function LoginPage() {
  const { login, isAuthenticated, initializing } = useAuth();
  const navigate = useNavigate();
  const [key, setKey] = useState("");
  const [persist, setPersist] = useState(false);
  const [showKey, setShowKey] = useState(false);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  // Already signed in (e.g. remembered key) — skip the login screen.
  // Must come after all hooks so hook order stays stable across renders.
  if (!initializing && isAuthenticated) {
    return <Navigate to="/" replace />;
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);

    const result = await login(key, persist);
    if (result.ok) {
      navigate("/");
    } else if (result.reason === "auth") {
      setError("Invalid admin key. Make sure PROXY_ADMIN_KEY matches.");
    } else {
      setError("Cannot reach the admin API. Is the server running on localhost:8081?");
    }
    setLoading(false);
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-4">
      <div className="absolute inset-0 overflow-hidden pointer-events-none">
        <div className="absolute -top-40 -right-40 h-80 w-80 rounded-full bg-primary/5 blur-3xl" />
        <div className="absolute -bottom-40 -left-40 h-80 w-80 rounded-full bg-primary/5 blur-3xl" />
      </div>

      <Card className="relative w-full max-w-sm border-border shadow-2xl shadow-black/40">
        <CardHeader className="items-center pb-2 pt-8">
          <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-lg bg-primary/12 ring-1 ring-inset ring-primary/30">
            <span className="grid grid-cols-2 gap-1">
              <i className="h-2 w-2 rounded-[2px] bg-primary" />
              <i className="h-2 w-2 rounded-[2px] bg-primary/40" />
              <i className="h-2 w-2 rounded-[2px] bg-primary/40" />
              <i className="h-2 w-2 rounded-[2px] bg-primary" />
            </span>
          </div>
          <CardTitle className="font-display text-xl">PiProxy Admin</CardTitle>
          <CardDescription>Enter your admin key to access the console</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="key">Admin Key</Label>
              <div className="relative">
                <Input
                  id="key"
                  type={showKey ? "text" : "password"}
                  placeholder="Enter your admin key..."
                  value={key}
                  onChange={(e) => setKey(e.target.value)}
                  className="pr-10 font-mono text-sm"
                  autoFocus
                />
                <button
                  type="button"
                  onClick={() => setShowKey(!showKey)}
                  aria-label={showKey ? "Hide admin key" : "Show admin key"}
                  className="absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                >
                  {showKey ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
                </button>
              </div>
            </div>

            <div className="flex items-center gap-2">
              <input
                type="checkbox"
                id="persist"
                checked={persist}
                onChange={(e) => setPersist(e.target.checked)}
                className="h-4 w-4 rounded border-border bg-transparent text-primary focus:ring-primary"
              />
              <Label htmlFor="persist" className="text-sm text-muted-foreground">
                Remember this key (stored in localStorage)
              </Label>
            </div>

            {error && (
              <div className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {error}
              </div>
            )}

            <Button type="submit" className="w-full" disabled={loading || !key.trim()}>
              {loading ? "Verifying..." : "Sign In"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
