export interface NodeInstallConfig {
  name: string;
  environment: string;
  release: string;
  controllerEndpointId: string;
  relayUrl: string;
  bootstrapTokenFile: string;
  relayAccessTokenFile: string;
  controllerCommandVerificationKeyFile: string;
}

export function validInstallField(
  key: keyof NodeInstallConfig,
  value: string,
): boolean {
  if (/\p{Cc}/u.test(value)) return false;
  switch (key) {
    case "name":
      return /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value);
    case "environment":
      return /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(value);
    case "release":
      return /^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$/.test(value);
    case "controllerEndpointId":
      return /^[0-9a-f]{64}$/.test(value);
    case "relayUrl": {
      if (!value.startsWith("https://") || /[\s"'`$\\?#@]/u.test(value))
        return false;
      try {
        const url = new URL(value);
        return (
          url.protocol === "https:" &&
          Boolean(url.hostname) &&
          !url.username &&
          !url.password
        );
      } catch {
        return false;
      }
    }
    default:
      return /^\/(?:[A-Za-z0-9_-][A-Za-z0-9._-]*\/)*[A-Za-z0-9_-][A-Za-z0-9._-]*$/.test(
        value,
      );
  }
}

export function validInstallConfig(config: NodeInstallConfig): boolean {
  return (
    (Object.keys(config) as (keyof NodeInstallConfig)[]).every((key) =>
      validInstallField(key, config[key]),
    ) &&
    new Set([
      config.bootstrapTokenFile,
      config.relayAccessTokenFile,
      config.controllerCommandVerificationKeyFile,
    ]).size === 3
  );
}

export function installEnv(
  config: NodeInstallConfig,
  workspaceId: string,
): string {
  if (
    !validInstallConfig(config) ||
    !/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
      workspaceId,
    )
  )
    return "";
  // Name/workspace bind the server-issued token; these are not installer keys.
  return `# Node: ${config.name}\n# Workspace: ${workspaceId}\n# Release: ${config.release}\nCONTROLLER_ENDPOINT_ID=${config.controllerEndpointId}\nENROLLMENT_ENVIRONMENT=${config.environment}\nRELAY_URL_A=${config.relayUrl}\nRELAY_URL_B=\nBOOTSTRAP_TOKEN_SOURCE=${config.bootstrapTokenFile}\nRELAY_ACCESS_TOKEN_SOURCE=${config.relayAccessTokenFile}\nCONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE=${config.controllerCommandVerificationKeyFile}\n`;
}

export function installCommand(release: string): string {
  if (!validInstallField("release", release)) return "";
  return `wget --https-only --secure-protocol=TLSv1_2 \\\n  https://github.com/GentleKingson/ocservia/releases/download/${release}/managed-node-bootstrap.sh \\\n  --output-document=install.sh\nbash ./install.sh --non-interactive --version ${release}`;
}
