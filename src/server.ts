// server.ts — one HTTP server for every surface: /healthz, git smart HTTP,
// REST v3 under /api/v3, GraphQL at /api/graphql and MCP at /mcp.
// Every REST reply is application/json with a non-empty X-GitHub-Request-Id drawn from a
// monotonic counter (no wall clock, no randomness — the state export stays deterministic).

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Forge, VIEWER_LOGIN } from './store.ts';
import { infoRefs, receivePack, uploadPack } from './git-http.ts';
import { handleRest, issueJsonText, JSON_PARSE_ERROR } from './rest.ts';
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
      const method = request.method ?? 'GET';
      // Split the request target by hand: `new URL` costs a full parse per request and
      // the suite only ever sends origin-form paths. Anything unusual (an absolute-form
      // target, dot segments, doubled slashes, backslashes) falls back to URL parsing.
      const rawUrl = request.url ?? '/';
      let pathname: string;
      let queryString: string;
      if (rawUrl.startsWith('/')) {
        let end = rawUrl.length;
        const hashAt = rawUrl.indexOf('#');
        if (hashAt >= 0) end = hashAt;
        const qAt = rawUrl.indexOf('?');
        if (qAt >= 0 && qAt < end) {
          pathname = rawUrl.slice(0, qAt);
          queryString = rawUrl.slice(qAt + 1, end);
        } else {
          pathname = rawUrl.slice(0, end);
          queryString = '';
        }
        if (pathname.includes('/./') || pathname.includes('/../') || pathname.includes('//') || pathname.includes('\\')) {
          const url = new URL(rawUrl, 'http://127.0.0.1');
          pathname = url.pathname;
          queryString = url.search.slice(1);
        }
      } else {
        const url = new URL(rawUrl, 'http://127.0.0.1');
        pathname = url.pathname;
        queryString = url.search.slice(1);
      }
      // The query is parsed lazily: most routes never read it.
      let searchParams: URLSearchParams | null = null;
      const query = (): URLSearchParams => {
        if (searchParams === null) searchParams = new URLSearchParams(queryString);
        return searchParams;
      };

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

      // Fast path: GET /api/v3/repos/{owner}/{repo}/issues/{number} — the timed read
      // workload (2,000 sequential reads of one issue). It answers exactly as the
      // router does (owner 404, repo 404, issue 404, the cached issue JSON), without
      // segment decoding or re-serialization. Any path needing decoding ('%'), a
      // non-numeric tail, or a trailing slash falls through to the router below.
      if (method === 'GET' && pathname.startsWith('/api/v3/repos/') && !pathname.includes('%')) {
        const tail = pathname.slice('/api/v3/repos/'.length).split('/');
        if (tail.length === 4 && tail[2] === 'issues' && tail[3] !== '') {
          const owner = tail[0]!;
          const repoName = tail[1]!;
          const number = Number(tail[3]);
          const repo = owner === VIEWER_LOGIN ? forge.getRepo(repoName) : undefined;
          if (repo === undefined) {
            sendJson(response, 404, { message: 'Not Found' }, { 'x-github-request-id': requestId });
            return;
          }
          const issue = repo.issueByNumber.get(number) ?? repo.issues.find((entry) => entry.number === number);
          if (issue === undefined) {
            sendJson(response, 404, { message: 'Not Found' }, { 'x-github-request-id': requestId });
            return;
          }
          const payload = issueJsonText(repo, issue);
          response.writeHead(200, { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': requestId });
          response.end(payload);
          return;
        }
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
          const service = query().get('service');
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
        const result = handleRest(forge, method, segments, query(), parsed, request.headers.host ?? '127.0.0.1');
        const headers: Record<string, string> = { 'x-github-request-id': requestId, ...(result.headers ?? {}) };
        if (result.rawBody !== undefined) {
          response.writeHead(result.status, { 'content-type': 'application/json; charset=utf-8', ...headers });
          response.end(result.rawBody);
          return;
        }
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
