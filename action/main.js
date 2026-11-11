'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');
const lib = require('./lib');

const env = process.env;

function appendCommandFile(file, values) {
  fs.appendFileSync(file, lib.commandFileLines(values));
}

function install() {
  const result = spawnSync('bash', [path.join(__dirname, 'install.sh'), path.join(env.RUNNER_TEMP, 'stratus-bin')], {
    env: {
      ...env,
      STRATUS_VERSION: lib.getInput(env, 'version') || 'latest',
      GH_TOKEN: lib.getInput(env, 'token'),
      ACTION_PATH: path.resolve(__dirname, '..'),
    },
    stdio: ['ignore', 'pipe', 'inherit'],
    encoding: 'utf8',
  });
  if (result.status !== 0) throw new Error('installing stratus failed');
  const binary = result.stdout.trim().split('\n').pop();
  fs.appendFileSync(env.GITHUB_PATH, `${path.dirname(binary)}\n`);
  return binary;
}

function runCommand(binary, run) {
  const args = lib.ciArgs({ logLevel: logLevel(), seed: lib.getInput(env, 'seed'), run });
  const result = spawnSync(binary, args, { stdio: 'inherit' });
  if (result.error) throw result.error;
  return result.status === null ? 1 : result.status;
}

async function startBackground(binary) {
  const port = lib.parsePort(lib.getInput(env, 'port')) ?? (await lib.freePort());
  const dataDir = path.join(env.RUNNER_TEMP, `stratus-data-${port}`);
  const logFile = path.join(env.RUNNER_TEMP, `stratus-${port}.log`);
  const log = fs.openSync(logFile, 'a');
  const args = lib.serveArgs({ port, dataDir, logLevel: logLevel(), seed: lib.getInput(env, 'seed') });
  const child = spawn(binary, args, { detached: true, stdio: ['ignore', log, log] });
  child.unref();
  appendCommandFile(env.GITHUB_STATE, { pid: child.pid, log: logFile });

  const endpoint = `http://127.0.0.1:${port}`;
  const wait = spawnSync(binary, ['wait', '--timeout', '60s'], {
    env: { ...env, STRATUS_ENDPOINT: endpoint },
    stdio: 'inherit',
  });
  if (wait.status !== 0) {
    process.stderr.write(fs.readFileSync(logFile, 'utf8'));
    throw new Error(`stratus did not become ready at ${endpoint}`);
  }

  appendCommandFile(env.GITHUB_ENV, lib.exportedEnv(endpoint, env));
  appendCommandFile(env.GITHUB_OUTPUT, { endpoint });
  console.log(`Stratus is ready at ${endpoint} (logs: ${logFile}); it is stopped when the job ends.`);
}

function logLevel() {
  return lib.getInput(env, 'log-level') || 'warn';
}

async function main() {
  const binary = install();
  const run = lib.getInput(env, 'run');
  if (run) {
    process.exitCode = runCommand(binary, run);
    return;
  }
  await startBackground(binary);
}

main().catch((err) => {
  console.log(`::error::${err.message}`);
  process.exitCode = 1;
});
