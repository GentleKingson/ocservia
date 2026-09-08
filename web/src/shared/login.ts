const oidcAttemptKey = "ocservia.login.oidc-attempt";

// UX state only. Authorization always comes from the server.
export function hasOIDCLoginAttempt(): boolean {
  try {
    return sessionStorage.getItem(oidcAttemptKey) !== null;
  } catch {
    return true; // Without persistent state, require an explicit SSO click.
  }
}

export function startOIDCLoginAttempt(): void {
  try {
    sessionStorage.setItem(oidcAttemptKey, "started");
  } catch {
    // Manual login remains available when browser storage is disabled.
  }
}

export function clearOIDCLoginAttempt(): void {
  try {
    sessionStorage.removeItem(oidcAttemptKey);
  } catch {
    // Storage availability is not an authorization decision.
  }
}

export function safeLoginReturnPath(
  value: string | undefined,
): string | undefined {
  if (
    !value?.startsWith("/") ||
    value.startsWith("//") ||
    value.includes("\\") ||
    Array.from(value).some(
      (char) => char.charCodeAt(0) <= 32 || char.charCodeAt(0) === 127,
    )
  )
    return undefined;
  const url = new URL(value, "https://console.invalid");
  if (
    url.origin !== "https://console.invalid" ||
    url.pathname.startsWith("//") ||
    url.pathname === "/login" ||
    url.pathname.startsWith("/api/")
  )
    return undefined;
  return `${url.pathname}${url.search}${url.hash}`;
}

export function retryAfterSeconds(value: string | null): number | undefined {
  if (!value) return undefined;
  const seconds = /^\d+$/.test(value)
    ? Number(value)
    : (Date.parse(value) - Date.now()) / 1000;
  return Number.isFinite(seconds) ? Math.max(0, Math.ceil(seconds)) : undefined;
}
