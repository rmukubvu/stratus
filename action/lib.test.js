'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const net = require('node:net');
const lib = require('./lib');

test('getInput reads INPUT_ variables, keeping hyphens and trimming', () => {
  const env = { 'INPUT_LOG-LEVEL': ' debug \n', INPUT_RUN: 'go test ./...' };
  assert.equal(lib.getInput(env, 'log-level'), 'debug');
  assert.equal(lib.getInput(env, 'run'), 'go test ./...');
  assert.equal(lib.getInput(env, 'seed'), '');
});

test('parsePort treats empty as "pick a free port"', () => {
  assert.equal(lib.parsePort(''), null);
  assert.equal(lib.parsePort('4566'), 4566);
});

test('parsePort rejects values that are not ports', () => {
  for (const bad of ['0', '65536', 'abc', '45.6', '-1']) {
    assert.throws(() => lib.parsePort(bad), /between 1 and 65535/, bad);
  }
});

test('ciArgs runs the command under bash, with an optional seed', () => {
  assert.deepEqual(lib.ciArgs({ logLevel: 'warn', seed: '', run: 'npm test' }), [
    'ci', '--log-level', 'warn', '--', 'bash', '-c', 'npm test',
  ]);
  assert.deepEqual(lib.ciArgs({ logLevel: 'info', seed: './snap', run: 'x' }), [
    'ci', '--log-level', 'info', '--seed', './snap', '--', 'bash', '-c', 'x',
  ]);
});

test('serveArgs passes port, data dir, log level and seed', () => {
  assert.deepEqual(lib.serveArgs({ port: 4577, dataDir: '/tmp/d', logLevel: 'warn', seed: '' }), [
    'serve', '--port', '4577', '--data-dir', '/tmp/d', '--log-level', 'warn',
  ]);
  assert.deepEqual(lib.serveArgs({ port: 1, dataDir: 'd', logLevel: 'warn', seed: 's' }).slice(-2), ['--seed', 's']);
});

test('exportedEnv points SDKs at Stratus and keeps credentials already set', () => {
  const vars = lib.exportedEnv('http://127.0.0.1:5000', { AWS_REGION: 'eu-west-1', AWS_ACCESS_KEY_ID: 'mine' });
  assert.equal(vars.AWS_ENDPOINT_URL, 'http://127.0.0.1:5000');
  assert.equal(vars.STRATUS_ENDPOINT, 'http://127.0.0.1:5000');
  assert.equal(vars.AWS_DISABLE_HOST_PREFIX_INJECTION, 'true');
  assert.equal(vars.AWS_ACCESS_KEY_ID, 'mine');
  assert.equal(vars.AWS_SECRET_ACCESS_KEY, 'test');
  assert.equal(vars.AWS_REGION, 'eu-west-1');
  assert.equal(vars.AWS_DEFAULT_REGION, 'us-east-1');
});

test('commandFileLines writes key=value lines and refuses multi-line values', () => {
  assert.equal(lib.commandFileLines({ a: 1, b: 'x' }), 'a=1\nb=x\n');
  assert.throws(() => lib.commandFileLines({ a: 'x\ny' }), /single line/);
});

test('freePort returns a port that can be bound', async () => {
  const port = await lib.freePort();
  assert.ok(port > 0 && port < 65536);
  await new Promise((resolve, reject) => {
    const server = net.createServer().once('error', reject).listen(port, () => server.close(resolve));
  });
});

function fakeProcess({ exitsAfterTerm }) {
  const signals = [];
  let alive = true;
  let polls = 0;
  return {
    signals,
    signal: (_pid, sig) => {
      signals.push(sig);
      if (sig === 'SIGKILL') alive = false;
    },
    isAlive: () => {
      if (signals.includes('SIGTERM') && exitsAfterTerm !== null && polls++ >= exitsAfterTerm) alive = false;
      return alive;
    },
    sleep: async () => {},
  };
}

test('stopServer sends SIGTERM and waits for a clean exit', async () => {
  const proc = fakeProcess({ exitsAfterTerm: 3 });
  const outcome = await lib.stopServer({ pid: 42, ...proc, timeoutMs: 10000, pollMs: 100 });
  assert.equal(outcome, 'stopped');
  assert.deepEqual(proc.signals, ['SIGTERM']);
});

test('stopServer kills a server that ignores SIGTERM past the timeout', async () => {
  const proc = fakeProcess({ exitsAfterTerm: null });
  const outcome = await lib.stopServer({ pid: 42, ...proc, timeoutMs: 1000, pollMs: 100 });
  assert.equal(outcome, 'killed');
  assert.deepEqual(proc.signals, ['SIGTERM', 'SIGKILL']);
});

test('stopServer does nothing when the server already exited', async () => {
  const proc = { signals: [], signal: () => assert.fail('signalled'), isAlive: () => false, sleep: async () => {} };
  assert.equal(await lib.stopServer({ pid: 42, ...proc }), 'not-running');
});

test('processAlive sees this process and not a free PID', () => {
  assert.equal(lib.processAlive(process.pid), true);
  assert.equal(lib.processAlive(2 ** 22 + 12345), false);
});
