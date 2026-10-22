export interface NodeInstallConfig {
  name: string;
  environment: string;
  release: string;
  controller: string;
  relay: string;
  bootstrapFile: string;
  relayFile: string;
  commandKeyFile: string;
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
    case "controller":
      return /^[0-9a-f]{64}$/.test(value);
    case "relay": {
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
    new Set([config.bootstrapFile, config.relayFile, config.commandKeyFile])
      .size === 3
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
  return `# Node: ${config.name}\n# Workspace: ${workspaceId}\n# Release: ${config.release}\nCONTROLLER_ENDPOINT_ID=${config.controller}\nENROLLMENT_ENVIRONMENT=${config.environment}\nRELAY_URL_A=${config.relay}\nRELAY_URL_B=\nBOOTSTRAP_TOKEN_SOURCE=${config.bootstrapFile}\nRELAY_ACCESS_TOKEN_SOURCE=${config.relayFile}\nCONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE=${config.commandKeyFile}\n`;
}

export function installCommand(release: string): string {
  if (!validInstallField("release", release)) return "";
  return `curl --fail --show-error --location --proto '=https' --proto-redir '=https' \\\n  https://raw.githubusercontent.com/GentleKingson/ocservia/${release}/deploy/managed-node/install.sh \\\n  --output install.sh\nbash ./install.sh --version ${release}`;
}
