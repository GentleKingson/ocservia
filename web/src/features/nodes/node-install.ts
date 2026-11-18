export interface NodeInstallConfig {
  name: string;
  environment: string;
  release: string;
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
  }
}

export function validInstallConfig(config: NodeInstallConfig): boolean {
  return (["name", "environment", "release"] as const).every((key) =>
    validInstallField(key, config[key]),
  );
}

export function installCommand(release: string): string {
  if (!validInstallField("release", release)) return "";
  return `wget --https-only --secure-protocol=TLSv1_2 \\\n  https://github.com/GentleKingson/ocservia/releases/download/${release}/managed-node-bootstrap.sh \\\n  --output-document=install.sh\nsudo bash ./install.sh --join-token --version ${release}`;
}
