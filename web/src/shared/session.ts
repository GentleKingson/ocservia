import {
  clearOIDCLoginAttempt,
  hasOIDCLoginAttempt,
  safeLoginReturnPath,
} from "./login";

const loginReturnKey = "ocservia.login.return-to";
let loginRedirecting = false;

// The return path is optional UX state; storage failures never block login.
function savedLoginReturnPath(): string | undefined {
  try {
    return sessionStorage.getItem(loginReturnKey) ?? undefined;
  } catch {
    return undefined;
  }
}

// Shared by the authenticated transport and the shell, never by a view import.
export function redirectToLogin(): void {
  if (typeof window === "undefined") return;
  if (!loginRedirecting && window.location.pathname !== "/login") {
    loginRedirecting = true;
    const returnTo = safeLoginReturnPath(
      `${window.location.pathname}${window.location.search}${window.location.hash}`,
    );
    if (returnTo && !safeLoginReturnPath(savedLoginReturnPath())) {
      try {
        sessionStorage.setItem(loginReturnKey, returnTo);
      } catch {
        // Login proceeds without a return path.
      }
    }
    window.location.assign(
      hasOIDCLoginAttempt() ? "/login?auth=failed" : "/login",
    );
  }
}

export function consumeLoginReturnPath(): string | undefined {
  const value = savedLoginReturnPath();
  try {
    sessionStorage.removeItem(loginReturnKey);
  } catch {
    // Nothing more can be cleared without storage.
  }
  clearOIDCLoginAttempt();
  return safeLoginReturnPath(value);
}
