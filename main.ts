// main.ts — the forge's entrypoint: bind the server, register its port with the trial env,
// then wait until the env says the trial is done and exit 0. Without an env (local runs)
// it simply serves on PORT for testing.
//
// The env lifecycle, as the kit settles it: bind an ephemeral port (never 8080, the env owns
// it), POST {"port": N} to $MAKEFUN_ENV_URL/register (200 wins, 409 means another entry won
// first and this process keeps waiting, 400/405 are fatal), then poll GET
// $MAKEFUN_ENV_URL/done until it answers {"done": true}. Retries use a short exponential
// backoff; the process never exits before the env says done.

import { createForgeServer } from './src/server.ts';
import { Forge } from './src/store.ts';

/** One register or done request may take this long before it is abandoned and retried. */
const REQUEST_BUDGET_MS = 30_000;
/** The first wait between retries, and the longest wait. */
const FIRST_RETRY_MS = 250;
const LONGEST_RETRY_MS = 2_000;

const wait = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `failures` failed attempts: doubling from FIRST_RETRY_MS, capped. */
function backoff(failures: number): number {
  return Math.min(LONGEST_RETRY_MS, FIRST_RETRY_MS * 2 ** Math.min(failures, 8));
}

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
const SHOP_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/**
 * Tell the env which port the forge bound. 200 is the normal answer; 409 means a first
 * register already won, which is not this process's to fix. 400 and 405 are contract
 * errors and end the program; anything else is retried, since the env may still be starting.
 */
async function announcePort(envUrl: string, port: number): Promise<void> {
  for (let failures = 0; ; failures += 1) {
    try {
      const answer = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
      });
      if (answer.status === 200 || answer.status === 409) return;
      if (answer.status === 400 || answer.status === 405) {
        throw new Error(`register answered HTTP ${answer.status}: ${await answer.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await wait(backoff(failures));
  }
}

/** Poll GET /done until it answers {"done": true}; never return before that. */
async function waitUntilDone(envUrl: string): Promise<void> {
  for (let failures = 0; ; failures += 1) {
    try {
      const answer = await fetch(`${envUrl}/done`, { signal: AbortSignal.timeout(REQUEST_BUDGET_MS) });
      if (answer.ok) {
        const body = (await answer.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await wait(backoff(failures));
  }
}

/** Bind the forge, register with the env when one exists, and run until the env says done. */
async function run(): Promise<void> {
  const forge = new Forge();
  const server = createForgeServer(forge, { token: SHOP_TOKEN === '' ? null : SHOP_TOKEN });

  if (ENV_URL === '') {
    // Local run: serve on PORT for testing, no env to register with.
    const port = Number(process.env.PORT ?? '8080');
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject);
      server.listen(port, '127.0.0.1', () => resolve());
    });
    process.stdout.write(`[forge] listening on http://127.0.0.1:${port} (no MAKEFUN_ENV_URL; local mode)\n`);
    // Stay alive serving requests until the process is stopped.
    await new Promise<void>(() => {});
    return;
  }

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('the server did not bind an ephemeral TCP port');
  process.stdout.write(`[forge] listening on port ${address.port}\n`);

  await announcePort(ENV_URL, address.port);
  process.stdout.write('[forge] registered; waiting for the env to say done\n');

  await waitUntilDone(ENV_URL);
  server.close();
  process.stdout.write('[forge] done\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
