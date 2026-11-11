'use strict';

const lib = require('./lib');

async function main() {
  const pid = Number(process.env.STATE_pid);
  if (!pid) return; // `run` mode, or the server never started.
  const outcome = await lib.stopServer({
    pid,
    signal: (target, sig) => process.kill(target, sig),
    isAlive: lib.processAlive,
    sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  });
  console.log(`Background Stratus (pid ${pid}): ${outcome}. Logs: ${process.env.STATE_log}`);
}

main().catch((err) => {
  // Cleanup must never fail the job.
  console.log(`::warning::could not stop Stratus: ${err.message}`);
});
