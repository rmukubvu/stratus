'use strict';

// Pure helpers behind the Setup Stratus action. Everything that touches the
// process, the file system or the network is passed in, so it can be tested.

const net = require('node:net');

/** Reads an action input the way the runner exposes it (INPUT_<NAME>). */
function getInput(env, name) {
  const value = env[`INPUT_${name.replace(/ /g, '_').toUpperCase()}`];
  return value === undefined ? '' : value.trim();
}

/** Parses the port input: empty means "pick a free one" (null). */
function parsePort(value) {
  if (value === '') return null;
  const port = Number(value);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`port must be a number between 1 and 65535, got "${value}"`);
  }
  return port;
}

/** Arguments for `stratus ci` running the user's command under bash. */
function ciArgs({ logLevel, seed, run }) {
  const args = ['ci', '--log-level', logLevel];
  if (seed) args.push('--seed', seed);
  return [...args, '--', 'bash', '-c', run];
}

/** Arguments for a background `stratus serve`. */
function serveArgs({ port, dataDir, logLevel, seed }) {
  const args = ['serve', '--port', String(port), '--data-dir', dataDir, '--log-level', logLevel];
  if (seed) args.push('--seed', seed);
  return args;
}

/**
 * Variables exported to later steps for a background instance. Credentials
 * and region keep any value the workflow already set.
 */
function exportedEnv(endpoint, env) {
  return {
    AWS_ENDPOINT_URL: endpoint,
    STRATUS_ENDPOINT: endpoint,
    AWS_DISABLE_HOST_PREFIX_INJECTION: 'true',
    AWS_ACCESS_KEY_ID: env.AWS_ACCESS_KEY_ID || 'test',
    AWS_SECRET_ACCESS_KEY: env.AWS_SECRET_ACCESS_KEY || 'test',
    AWS_REGION: env.AWS_REGION || 'us-east-1',
    AWS_DEFAULT_REGION: env.AWS_DEFAULT_REGION || 'us-east-1',
  };
}

/** Formats key/value pairs for $GITHUB_ENV, $GITHUB_OUTPUT or $GITHUB_STATE. */
function commandFileLines(values) {
  return Object.entries(values)
    .map(([key, value]) => {
      const text = String(value);
      if (/[\r\n]/.test(text)) throw new Error(`${key} must be a single line`);
      return `${key}=${text}\n`;
    })
    .join('');
}

/**
 * Asks the OS for a free TCP port on every interface, the way `stratus serve`
 * listens. Concurrent jobs on one runner host each get their own port.
 */
function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.unref();
    server.once('error', reject);
    server.listen(0, () => {
      const { port } = server.address();
      server.close(() => resolve(port));
    });
  });
}

/**
 * Stops a background Stratus so it removes its containers before the job
 * ends. The runner itself would kill it with SIGKILL, which leaves Lambda and
 * engine containers running until the next Stratus start reaps them.
 *
 * SIGTERM lets Stratus shut down cleanly on Linux and macOS. Windows has no
 * graceful signal for a detached process, so it is terminated outright; the
 * next start on that runner reaps whatever it left behind.
 */
async function stopServer({ pid, signal, isAlive, sleep, timeoutMs = 30000, pollMs = 250 }) {
  if (!isAlive(pid)) return 'not-running';
  signal(pid, 'SIGTERM');
  for (let waited = 0; waited < timeoutMs; waited += pollMs) {
    if (!isAlive(pid)) return 'stopped';
    await sleep(pollMs);
  }
  signal(pid, 'SIGKILL');
  return 'killed';
}

/** Reports whether a process exists, without signalling it. */
function processAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err.code === 'EPERM';
  }
}

module.exports = {
  getInput,
  parsePort,
  ciArgs,
  serveArgs,
  exportedEnv,
  commandFileLines,
  freePort,
  stopServer,
  processAlive,
};
