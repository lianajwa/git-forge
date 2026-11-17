// server.ts — one HTTP server for every surface: /healthz, git smart HTTP,
// REST v3 under /api/v3, GraphQL at /api/graphql and MCP at /mcp.
// Every REST reply is application/json with a non-empty X-GitHub-Request-Id drawn from a
// monotonic counter (no wall clock, no randomness — the state export stays deterministic).

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Forge } from './store.ts';
import { infoRefs, receivePack, uploadPack } from './git-http.ts';
import { handleRest, JSON_PARSE_ERROR } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const MAX_BODY_BYTES = 64 * 1024 * 1024;

export interface ServerOptions {
  /** The Bearer token that authorizes every call except /healthz. Null accepts any token. */
  token: string | null;
}

function readBody(request: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    request.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        request.destroy();
        return;
      }
      chunks.push(chunk);
    });
    request.on('end', () => resolve(Buffer.concat(chunks)));
    request.on('error', reject);
  });
}

function sendJson(response: ServerResponse, status: number, body: unknown, extraHeaders: Record<string, string> = {}): void {
  const payload = body === undefined ? '' : JSON.stringify(body);
  response.writeHead(status, { 'content-type': 'application/json; charset=utf-8', ...extraHeaders });
  response.end(payload);
}

function decodeSegments(pathname: string): string[] | null {
  const parts = pathname.split('/').filter((part) => part !== '');
  const decoded: string[] = [];
  for (const part of parts) {
    try {
      decoded.push(decodeURIComponent(part));
    } catch {
      return null;
    }
  }
  return decoded;
}

export function createForgeServer(forge: Forge, options: ServerOptions): Server {
  let requestCounter = 0;

  const server = createServer(async (request: IncomingMessage, response: ServerResponse) => {
    const requestId = `req-${++requestCounter}`;
    try {
      const url = new URL(request.url ?? '/', 'http://127.0.0.1');
      const method = request.method ?? 'GET';
      const pathname = url.pathname;

      // /healthz answers without a token; the harness pings it twice at boot.
      if (pathname === '/healthz') {
        if (method !== 'GET') {
          sendJson(response, 405, { message: 'Method Not Allowed' });
          return;
        }
        sendJson(response, 200, { ok: true });
        return;
      }

      // Every other surface needs a Bearer token.
      const authorization = request.headers.authorization ?? '';
      const bearer = authorization.startsWith('Bearer ') ? authorization.slice('Bearer '.length) : '';
      if (bearer === '' || (options.token !== null && bearer !== options.token)) {
        sendJson(response, 401, { message: 'Unauthorized' }, { 'x-github-request-id': requestId });
        return;
      }

      const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(request);

      // git smart HTTP: /{owner}/{repo}.git/{info/refs, git-upload-pack, git-receive-pack}
      const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(pathname);
      if (gitMatch !== null) {
        const [, owner, repoName, action] = gitMatch;
        if (action === 'info/refs') {
          if (method !== 'GET') {
            sendJson(response, 405, { message: 'Method Not Allowed' });
            return;
          }
          const service = url.searchParams.get('service');
          const gitProtocol = request.headers['git-protocol'] ?? null;
          const result = infoRefs(forge, owner!, repoName!, service, gitProtocol);
          response.writeHead(result.status, { 'content-type': result.contentType, 'x-github-request-id': requestId });
          response.end(result.body);
          return;
        }
        if (method !== 'POST') {
          sendJson(response, 405, { message: 'Method Not Allowed' });
          return;
        }
        const result = action === 'git-upload-pack' ? uploadPack(forge, owner!, repoName!, body) : receivePack(forge, owner!, repoName!, body);
        response.writeHead(result.status, { 'content-type': result.contentType, 'x-github-request-id': requestId });
        response.end(result.body);
        return;
      }

      // GraphQL.
      if (pathname === '/api/graphql') {
        if (method !== 'POST') {
          sendJson(response, 405, { message: 'Method Not Allowed' }, { 'x-github-request-id': requestId });
          return;
        }
        let parsed: unknown;
        try {
          parsed = body.length === 0 ? {} : JSON.parse(body.toString('utf8'));
        } catch {
          sendJson(response, 400, { errors: [{ message: 'Invalid JSON' }] }, { 'x-github-request-id': requestId });
          return;
        }
        const result = handleGraphql(forge, parsed);
        sendJson(response, result.status, result.body, { 'x-github-request-id': requestId });
        return;
      }

      // MCP.
      if (pathname === '/mcp') {
        if (method !== 'POST') {
          sendJson(response, 405, { message: 'Method Not Allowed' }, { 'x-github-request-id': requestId });
          return;
        }
        let parsed: unknown;
        try {
          parsed = body.length === 0 ? {} : JSON.parse(body.toString('utf8'));
        } catch {
          sendJson(
            response,
            400,
            { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } },
            { 'x-github-request-id': requestId },
          );
          return;
        }
        const result = handleMcp(forge, parsed);
        sendJson(response, result.status, result.body, { 'x-github-request-id': requestId });
        return;
      }

      // REST v3 under /api/v3.
      if (pathname === '/api/v3' || pathname.startsWith('/api/v3/')) {
        const segments = decodeSegments(pathname.replace(/^\/api\/v3/, ''));
        if (segments === null) {
          sendJson(response, 400, { message: 'Invalid URL encoding' }, { 'x-github-request-id': requestId });
          return;
        }
        let parsed: unknown;
        if (body.length === 0) {
          parsed = undefined;
        } else {
          try {
            parsed = JSON.parse(body.toString('utf8'));
          } catch {
            parsed = JSON_PARSE_ERROR;
          }
        }
        const result = handleRest(forge, method, segments, url.searchParams, parsed, request.headers.host ?? '127.0.0.1');
        const headers: Record<string, string> = { 'x-github-request-id': requestId, ...(result.headers ?? {}) };
        if (result.body === undefined) {
          response.writeHead(result.status, headers);
          response.end();
          return;
        }
        sendJson(response, result.status, result.body, headers);
        return;
      }

      sendJson(response, 404, { message: 'Not Found' }, { 'x-github-request-id': requestId });
    } catch (err) {
      process.stderr.write(`forge error: ${err instanceof Error ? err.message : String(err)}\n`);
      if (!response.headersSent) {
        sendJson(response, 500, { message: 'Internal Server Error' }, { 'x-github-request-id': requestId });
      } else {
        response.end();
      }
    }
  });

  return server;
}
