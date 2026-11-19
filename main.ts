// main.ts — the forge's entrypoint for the trial.
//
// The trial runs `node main.ts`. This file starts the Go core, learns the port
// it bound from the `FORGE_LISTEN <port>` line it prints, registers that port
// with the trial env (when MAKEFUN_ENV_URL is set), and waits until the env
// answers done, then stops the core and exits 0. With no env set it simply
// serves locally until stopped.
//
// The core binary ships in ./go/forge. Web-UI uploads lose the exec bit and
// /bot may be read-only in a trial, so the binary is copied to a fresh temp
// dir and made executable there before it is spawned.

import { spawn, type ChildProcess } from 'node:child_process';
import { chmod, copyFile, mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const REQUEST_BUDGET_MS = 30_000;
const FIRST_RETRY_MS = 250;
const LONGEST_RETRY_MS = 2_000;

const wait = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

function backoff(failures: number): number {
  return Math.min(LONGEST_RETRY_MS, FIRST_RETRY_MS * 2 ** Math.min(failures, 8));
}

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

/** Register the bound port. 200 is normal; 409 means another entry won first. */
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

/** Poll GET /done until it answers {done: true}; never return before that. */
async function waitUntilDone(envUrl: string): Promise<void> {
  for (let failures = 0; ; failures += 1) {
    try {
      const answer = await fetch(`${envUrl}/done`, {
        signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
      });
      if (answer.ok) {
        const body = (await answer.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet: it is not done, so poll again.
    }
    await wait(backoff(failures));
  }
}

/** Copy the core to a temp dir, make it executable, spawn it, read its port. */
async function launchCore(): Promise<{ child: ChildProcess; port: number }> {
  const dir = await mkdtemp(join(tmpdir(), 'forge-'));
  const binPath = join(dir, 'forge');
  await copyFile(new URL('./go/forge', import.meta.url), binPath);
  await chmod(binPath, 0o755);
  const child = spawn(binPath, [], { stdio: ['ignore', 'pipe', 'inherit'], env: process.env });
  const port = await new Promise<number>((resolve, reject) => {
    let buffer = '';
    let settled = false;
    child.stdout!.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      const newline = buffer.indexOf('\n');
      if (newline < 0) return;
      const line = buffer.slice(0, newline).trim();
      const match = /^FORGE_LISTEN (\d+)$/.exec(line);
      if (match === null) {
        if (!settled) {
          settled = true;
          reject(new Error(`unexpected core output: ${line}`));
        }
        return;
      }
      if (!settled) {
        settled = true;
        resolve(Number(match[1]));
      }
    });
    child.once('close', (code) => {
      if (!settled) {
        settled = true;
        reject(new Error(`core exited before listening (code ${code})`));
      }
    });
    child.once('error', (err) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    });
  });
  return { child, port };
}

async function run(): Promise<void> {
  const { child, port } = await launchCore();
  process.once('SIGINT', () => {
    child.kill();
    process.exit(130);
  });
  process.once('SIGTERM', () => {
    child.kill();
    process.exit(143);
  });

  if (ENV_URL === '') {
    process.stdout.write(`[forge] listening on http://127.0.0.1:${port} (no MAKEFUN_ENV_URL; local mode)\n`);
    await new Promise<void>(() => {});
    return;
  }

  process.stdout.write(`[forge] listening on port ${port}\n`);
  await announcePort(ENV_URL, port);
  process.stdout.write('[forge] registered; waiting for the env to say done\n');
  await waitUntilDone(ENV_URL);
  child.kill();
  await new Promise<void>((resolve) => child.once('close', () => resolve()));
  process.stdout.write('[forge] done\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
