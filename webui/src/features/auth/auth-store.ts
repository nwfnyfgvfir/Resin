import { create } from "zustand";

const TOKEN_KEY = "resin_admin_token";

function loadInitialToken(): string {
  if (typeof window === "undefined") {
    return "";
  }
  return window.localStorage.getItem(TOKEN_KEY) ?? "";
}

type AuthState = {
  token: string;
  setToken: (token: string) => void;
  clearToken: () => void;
};

export const useAuthStore = create<AuthState>((set) => ({
  token: loadInitialToken(),
  setToken: (token) => {
    const next = token.trim();
    if (typeof window !== "undefined") {
      window.localStorage.setItem(TOKEN_KEY, next);
    }
    set({ token: next });
  },
  clearToken: () => {
    if (typeof window !== "undefined") {
      window.localStorage.removeItem(TOKEN_KEY);
    }
    set({ token: "" });
  },
}));

export function getStoredAuthToken(): string {
  return useAuthStore.getState().token;
}

type UnauthorizedHandler = () => void;

let unauthorizedHandler: UnauthorizedHandler | null = null;

/**
 * Registers a callback invoked when the admin API rejects the stored token.
 * The handler is expected to route the UI back to the login page.
 */
export function setUnauthorizedHandler(handler: UnauthorizedHandler | null): void {
  unauthorizedHandler = handler;
}

/**
 * Handles an admin-API 401 for a request made with the stored token.
 *
 * Clears the token from both localStorage and the in-memory Zustand store
 * (so a cleared site-data state cannot keep using a stale token), then notifies
 * the registered handler so the app can return to the login page.
 *
 * Idempotent: once the token is cleared, subsequent requests no longer carry it
 * and will not re-trigger this path.
 */
export function handleUnauthorized(): void {
  useAuthStore.getState().clearToken();
  unauthorizedHandler?.();
}
