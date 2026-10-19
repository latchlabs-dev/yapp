import {
  createContext,
  useContext,
  useState,
  useCallback,
  useEffect,
  type ReactNode,
} from "react";
import {
  AUTH_EXPIRED_EVENT,
  clearStoredAdminKey,
  getStoredAdminKey,
  setStoredAdminKey,
} from "@/lib/auth-session";

interface AuthContextType {
  adminKey: string | null;
  isAuthenticated: boolean;
  /** True until the stored key (if any) has been verified against the server. */
  initializing: boolean;
  login: (
    key: string,
    persist?: boolean,
  ) => Promise<{ ok: boolean; reason?: "auth" | "unreachable" }>;
  logout: () => void;
}

const AuthContext = createContext<AuthContextType | null>(null);

function readStoredKey(): string | null {
  return getStoredAdminKey();
}

export function AuthProvider({ children }: { children: ReactNode }) {
  // Initialize optimistically from storage so a remembered key keeps the user
  // signed in across reloads instead of bouncing to /login while we verify.
  const [adminKey, setAdminKey] = useState<string | null>(() => readStoredKey());
  // Only "initializing" when there's a stored key to verify.
  const [initializing, setInitializing] = useState<boolean>(() => !!readStoredKey());

  // Verify the stored key in the background. If the server rejects it (e.g. the
  // admin key changed on an upgrade/redeploy), drop it so the user is taken back
  // to the login screen and can enter the new one.
  useEffect(() => {
    const stored = readStoredKey();
    if (!stored) {
      setInitializing(false);
      return;
    }
    let cancelled = false;
    fetch("/api/tenants", { headers: { "X-Admin-Key": stored } })
      .then((res) => {
        if (cancelled) return;
        if (res.status === 401 || res.status === 403) {
          // Wrong/stale key — auto-remove so it can be replaced.
          clearStoredAdminKey();
          setAdminKey(null);
        } else {
          // Reachable (or transient server error) — keep the key.
          setAdminKey(stored);
        }
      })
      .catch(() => {
        // Server unreachable — keep the key; the user will see request errors.
      })
      .finally(() => {
        if (!cancelled) setInitializing(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    const onAuthExpired = () => {
      setAdminKey(null);
    };
    window.addEventListener(AUTH_EXPIRED_EVENT, onAuthExpired);
    return () => window.removeEventListener(AUTH_EXPIRED_EVENT, onAuthExpired);
  }, []);

  const login = useCallback(
    async (
      key: string,
      persist = false,
    ): Promise<{ ok: boolean; reason?: "auth" | "unreachable" }> => {
      try {
        const res = await fetch("/api/tenants", {
          headers: { "X-Admin-Key": key },
        });

        // 401 or 403 = wrong key
        if (res.status === 401 || res.status === 403) {
          return { ok: false, reason: "auth" };
        }

        // Any other non-ok status = server issue (502 proxy error, 5xx, etc.)
        if (!res.ok) {
          return { ok: false, reason: "unreachable" };
        }

        // Persist to the chosen store; clear the other so they never disagree.
        setStoredAdminKey(key, persist);
        setAdminKey(key);
        return { ok: true };
      } catch {
        return { ok: false, reason: "unreachable" };
      }
    },
    [],
  );

  const logout = useCallback(() => {
    clearStoredAdminKey();
    setAdminKey(null);
  }, []);

  return (
    <AuthContext.Provider
      value={{
        adminKey,
        isAuthenticated: adminKey !== null,
        initializing,
        login,
        logout,
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
