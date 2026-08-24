// The GitHub runner injects ACTIONS_RUNTIME_TOKEN and the cache-service
// URLs only into action processes, not into plain run steps
// (actions/runner#4325 is still open). This relay action copies them into
// the job environment through the GITHUB_ENV multiline protocol so the
// release jobs can inject them into their buildkitd builder. The values
// are never logged or exported as step outputs. Cache persistence is
// optional, so a missing runtime token marks the cache unavailable and lets
// the caller continue with a cold build.
'use strict';

const fs = require('fs');

const relayed = ['ACTIONS_RUNTIME_TOKEN', 'ACTIONS_CACHE_URL', 'ACTIONS_RESULTS_URL'];
const envFile = process.env.GITHUB_ENV;
if (!envFile) {
  console.warn('GITHUB_ENV is not available; continuing without BuildKit cache credentials');
  process.exit(0);
}

let payload = '';
const values = new Map();
for (const name of relayed) {
  const value = process.env[name];
  if (!value) {
    continue;
  }
  values.set(name, value);
  payload += `${name}<<G6_CACHE_CREDENTIALS_EOF\n${value}\nG6_CACHE_CREDENTIALS_EOF\n`;
  console.log(`relayed ${name} to the job environment`);
}
const cacheAvailable =
  values.has('ACTIONS_RUNTIME_TOKEN') &&
  (values.has('ACTIONS_CACHE_URL') || values.has('ACTIONS_RESULTS_URL'));
payload += `G6_CACHE_AVAILABLE<<G6_CACHE_CREDENTIALS_EOF\n${cacheAvailable}\nG6_CACHE_CREDENTIALS_EOF\n`;

try {
  fs.appendFileSync(envFile, payload);
} catch (error) {
  const message = error instanceof Error ? error.message : String(error);
  console.warn(`could not relay BuildKit cache credentials; continuing without cache: ${message}`);
  process.exit(0);
}

if (!cacheAvailable) {
  console.warn('BuildKit cache credentials are unavailable; continuing with a cold build');
}
