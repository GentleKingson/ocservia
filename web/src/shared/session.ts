import {
  clearOIDCLoginAttempt,
  hasOIDCLoginAttempt,
  safeLoginReturnPath,
} from "./login";

const loginReturnKey = "ocservia.login.return-to";
let loginRedirecting = false;

// Shared by the authenticated transport and the shell, never by a view import.
export function redirectToLogin(): void {
  if (typeof window === "undefined") return;
  if (!loginRedirecting && window.location.pathname !== "/login") {
    loginRedirecting = true;
    const returnTo = safeLoginReturnPath(
      `${window.location.pathname}${window.location.search}${window.location.hash}`,
    );
    if (
      returnTo &&
      !safeLoginReturnPath(sessionStorage.getItem(loginReturnKey) ?? undefined)
    ) {
      sessionStorage.setItem(loginReturnKey, returnTo);
    }
    window.location.assign(
      hasOIDCLoginAttempt() ? "/login?auth=failed" : "/login",
    );
  }
}

export function consumeLoginReturnPath(): string | undefined {
  const value = sessionStorage.getItem(loginReturnKey) ?? undefined;
  sessionStorage.removeItem(loginReturnKey);
  clearOIDCLoginAttempt();
  return safeLoginReturnPath(value);
}
