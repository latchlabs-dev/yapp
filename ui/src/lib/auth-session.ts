const STORAGE_KEY = "piproxy-admin-key";

export const AUTH_EXPIRED_EVENT = "piproxy-auth-expired";

export function getStoredAdminKey(): string | null {
  return (
    localStorage.getItem(STORAGE_KEY) ||
    sessionStorage.getItem(STORAGE_KEY) ||
    null
  );
}

export function setStoredAdminKey(key: string, persist: boolean): void {
  clearStoredAdminKey();
  if (persist) {
    localStorage.setItem(STORAGE_KEY, key);
  } else {
    sessionStorage.setItem(STORAGE_KEY, key);
  }
}

export function clearStoredAdminKey(): void {
  localStorage.removeItem(STORAGE_KEY);
  sessionStorage.removeItem(STORAGE_KEY);
}

export function notifyAuthExpired(): void {
  clearStoredAdminKey();
  window.dispatchEvent(new Event(AUTH_EXPIRED_EVENT));
}
