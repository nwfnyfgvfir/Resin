import { useEffect } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { setUnauthorizedHandler } from "./auth-store";

/**
 * Bridges admin-API 401 responses to a router navigation.
 *
 * When the stored admin token is rejected (for example after RESIN_ADMIN_TOKEN
 * was rotated and the container restarted), `handleUnauthorized` clears the
 * session and calls the handler registered here, which returns the user to the
 * login page while preserving the current location as `next`.
 *
 * Renders nothing; it only installs the handler.
 */
export function AuthUnauthorizedBridge() {
  const navigate = useNavigate();
  const location = useLocation();

  useEffect(() => {
    setUnauthorizedHandler(() => {
      // Already on the login page: nothing to redirect.
      if (location.pathname.startsWith("/login")) {
        return;
      }
      const next = `${location.pathname}${location.search}`;
      navigate(`/login?next=${encodeURIComponent(next)}`, { replace: true });
    });
    return () => setUnauthorizedHandler(null);
  }, [navigate, location.pathname, location.search]);

  return null;
}
