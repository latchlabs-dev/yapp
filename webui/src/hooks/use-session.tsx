import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { api, ApiError, SESSION_EXPIRED_EVENT, setCsrfToken } from "@/lib/api";
import type { Me } from "@/lib/types";

interface SessionContextValue {
  me: Me | null;
  /** True until the first /me call settles, so the app can avoid flashing login. */
  loading: boolean;
  signOut: () => Promise<boolean>;
  signingOut: boolean;
  signOutError: string;
  refresh: () => Promise<boolean>;
}

const SessionContext = createContext<SessionContextValue | null>(null);

/**
 * SessionProvider resolves the browser session from the cookie.
 *
 * There is no client-side token to inspect: the cookie is HttpOnly, so the only
 * way to know whether the user is signed in is to ask the server. That is why
 * the app renders nothing until /me settles rather than guessing.
 */
export function SessionProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);
  const [signingOut, setSigningOut] = useState(false);
  const [signOutError, setSignOutError] = useState("");
  const logoutInFlight = useRef(false);
  const generation = useRef(0);

  const refresh = useCallback(async () => {
    const current = ++generation.current;
    try {
      const session = await api.me();
      if (current !== generation.current) return false;
      setCsrfToken(session.csrf_token);
      setMe(session);
      return true;
    } catch {
      if (current === generation.current) {
        setCsrfToken("");
        setMe(null);
      }
      return false;
    } finally {
      if (current === generation.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // A 401 from any request means the session went away underneath us — an
  // expiry, a logout in another tab, or a revoked account.
  useEffect(() => {
    const onExpired = () => {
      generation.current++;
      setLoading(false);
      setCsrfToken("");
      setMe(null);
    };
    window.addEventListener(SESSION_EXPIRED_EVENT, onExpired);
    return () => window.removeEventListener(SESSION_EXPIRED_EVENT, onExpired);
  }, []);

  const signOut = useCallback(async () => {
    if (logoutInFlight.current) return false;
    logoutInFlight.current = true;
    generation.current++; // Older /me responses must not undo a completed logout.
    setSigningOut(true);
    setSignOutError("");
    try {
      try {
        await api.logout();
      } catch (error) {
        if (!(error instanceof ApiError) || error.status !== 403) throw error;
        // Another tab may have replaced the cookie/CSRF pair. Refresh the
        // token and retry ONCE, retaining all server-side CSRF enforcement.
        const session = await api.me(false);
        setCsrfToken(session.csrf_token);
        setMe(session);
        await api.logout();
      }
      generation.current++;
      setCsrfToken("");
      setMe(null);
      return true;
    } catch {
      setSignOutError("Sign-out could not be confirmed. You may still be signed in. Please try again.");
      return false;
    } finally {
      logoutInFlight.current = false;
      setSigningOut(false);
    }
  }, []);

  return (
    <SessionContext.Provider value={{ me, loading, signOut, signingOut, signOutError, refresh }}>
      {children}
    </SessionContext.Provider>
  );
}

export function useSession() {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error("useSession must be used inside SessionProvider");
  return ctx;
}
