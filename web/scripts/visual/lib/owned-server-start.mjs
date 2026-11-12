/**
 * Starts the one server a script owns and waits until it answers.
 *
 * A script that polls `/api/v1/health` after `spawn` cannot tell its own server
 * from one that was already on the port: an earlier run's server answers, the
 * new one fails to bind and exits, and the script goes on to drive the old one,
 * on whatever store that one opened. So the port is checked first and the start
 * is refused when anything answers, and the wait ends as soon as the started
 * process has exited.
 */

const pause = (ms) => new Promise((done) => setTimeout(done, ms));

async function healthAnswers(origin) {
  try {
    return (await fetch(`${origin}/api/v1/health`, { signal: AbortSignal.timeout(1000) })).ok;
  } catch {
    return false;
  }
}

const exited = (server) => server.exitCode !== null || server.signalCode !== null;
const how = (server) => (server.signalCode !== null ? `was ended by ${server.signalCode}` : `exited with status ${server.exitCode}`);

/**
 * @param {object} options
 * @param {string | number} options.port the port the script starts its server on
 * @param {string} options.portVariable the environment variable of the script that picks another port
 * @param {() => import('node:child_process').ChildProcess} options.start spawns the server; called only once the port is free
 * @param {number} [options.withinMs] how long the server has to answer
 * @param {string} [options.log] where the server's output goes, when the script writes it to a file
 * @returns {Promise<import('node:child_process').ChildProcess>} the started server, answering
 */
export async function startOwnedServer({ port, portVariable, start, withinMs = 20000, log }) {
  const origin = `http://localhost:${port}`;
  if (await healthAnswers(origin)) {
    throw new Error(`The server was not started in startOwnedServer because something already answers ${origin}/api/v1/health. It is not this run's server, so the script would drive a server on a store this run does not own. Nothing was started. Stop the process listening on port ${port} (find it with: ss -ltnp 'sport = :${port}'), or set ${portVariable} to a free port, then run the script again.`);
  }
  const server = start();
  let output = '';
  let spawnError = null;
  server.stderr?.on('data', (data) => { output = (output + data).slice(-4000); });
  server.once('error', (error) => { spawnError = error; });
  const said = () => (log ? ` Its output is in ${log}.` : output.trim() ? ` It wrote: ${output.trim()}` : '');
  const deadline = Date.now() + withinMs;
  while (Date.now() < deadline) {
    if (spawnError) throw new Error(`The server could not be started in startOwnedServer because the binary did not run: ${spawnError.message}. Nothing is listening on ${origin}. Build the binary (make build) or point the script at an existing one, then run it again.`);
    if (exited(server)) throw new Error(`The server did not come up in startOwnedServer because the process this run started ${how(server)} before it answered ${origin}/api/v1/health.${said()} Nothing of this run is listening on port ${port}. Fix what it reports (a port taken since the check, a build that does not start) and run the script again.`);
    if (await healthAnswers(origin)) {
      // An answer counts only while this run's process is still alive a moment later:
      // a server that lost the port to another process exits right after starting.
      await pause(300);
      if (!exited(server)) return server;
      continue;
    }
    await pause(100);
  }
  server.kill('SIGKILL');
  throw new Error(`The server did not come up in startOwnedServer because the process this run started did not answer ${origin}/api/v1/health within ${withinMs} ms.${said()} It was stopped. Run the same command by hand to see where it waits, then run the script again.`);
}
