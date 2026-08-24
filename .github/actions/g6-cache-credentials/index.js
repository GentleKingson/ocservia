// The GitHub runner injects ACTIONS_RUNTIME_TOKEN and the cache-service
// URLs only into action processes, not into plain run steps
// (actions/runner#4325 is still open). This relay action copies them into
// the job environment through the GITHUB_ENV multiline protocol so the
// release jobs can inject them into their buildkitd builder. The values
// are never logged or exported as step outputs; a missing runtime token
// fails the action so BuildKit cache persistence cannot silently no-op.
'use strict';

const fs = require('fs');

const relayed = ['ACTIONS_RUNTIME_TOKEN', 'ACTIONS_CACHE_URL', 'ACTIONS_RESULTS_URL'];
const envFile = process.env.GITHUB_ENV;
if (!envFile) {
  console.error('GITHUB_ENV is not available; cannot relay cache credentials');
  process.exit(1);
}

let payload = '';
let tokenRelayed = false;
for (const name of relayed) {
  const value = process.env[name];
  if (!value) {
    continue;
  }
  payload += `${name}<<G6_CACHE_CREDENTIALS_EOF\n${value}\nG6_CACHE_CREDENTIALS_EOF\n`;
  console.log(`relayed ${name} to the job environment`);
  if (name === 'ACTIONS_RUNTIME_TOKEN') {
    tokenRelayed = true;
  }
}
fs.appendFileSync(envFile, payload);

if (!tokenRelayed) {
  console.error(
    'ACTIONS_RUNTIME_TOKEN is not available to actions; ' +
      'BuildKit cache persistence would silently no-op'
  );
  process.exit(1);
}
