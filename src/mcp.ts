// mcp.ts — Model Context Protocol over JSON-RPC 2.0 at /mcp (modern era 2026-07-28).
// Serves server/discover, initialize, tools/list and tools/call with six tools that wrap
// the same Forge store as REST, git and GraphQL. Tool results carry both structuredContent
// and a JSON text block, so either reading path the client prefers works.

import { DEFAULT_BRANCH, Forge, VIEWER_LOGIN, type Issue, type Pull, type Repo } from './store.ts';
import { issueJson, pullJson, repoJson } from './rest.ts';

export interface McpResult {
  status: number;
  body: unknown;
}

const SERVER_NAME = 'git-forge';
const SERVER_VERSION = '1.0.0';
const PROTOCOL_VERSION = '2026-07-28';

interface ToolDef {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, unknown>; required: string[] };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the viewer.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
        labels: { type: 'array', items: { type: 'string' }, description: 'Label names to attach.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Filter by issue state.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from one branch onto another.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function rpcResult(id: unknown, result: unknown): McpResult {
  return { status: 200, body: { jsonrpc: '2.0', id, result } };
}

function rpcError(id: unknown, code: number, message: string, status = 200): McpResult {
  return { status, body: { jsonrpc: '2.0', id, error: { code, message } } };
}

/** A successful tool result: JSON text plus the same object as structuredContent. */
function toolOk(result: unknown): Record<string, unknown> {
  const text = JSON.stringify(result);
  return { content: [{ type: 'text', text }], structuredContent: result, isError: false };
}

/** A failed tool result: isError true with the reason as text. */
function toolFail(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

/** Read an argument by its canonical name, falling back to the suite's synonyms. */
function pickArg(args: Record<string, unknown>, ...names: string[]): unknown {
  for (const name of names) {
    const value = args[name];
    if (value !== undefined && value !== null) return value;
  }
  return undefined;
}

function missingRequired(args: Record<string, unknown>, required: string[]): string | null {
  for (const name of required) {
    const value = args[name];
    if (value === undefined || value === null || value === '') return name;
  }
  return null;
}

function callTool(forge: Forge, name: string, argsRaw: unknown): Record<string, unknown> {
  const args: Record<string, unknown> = argsRaw !== null && typeof argsRaw === 'object' && !Array.isArray(argsRaw)
    ? (argsRaw as Record<string, unknown>)
    : {};

  const owner = pickArg(args, 'owner', 'owner_login', 'org', 'login') as string | undefined;
  const repoName = pickArg(args, 'repo', 'repository', 'repo_name') as string | undefined;

  switch (name) {
    case 'create_repo': {
      const missing = missingRequired(args, ['name']);
      if (missing !== null) return toolFail(`Missing required argument: ${missing}`);
      const repoNameArg = args.name as string;
      if (forge.getRepo(repoNameArg) !== undefined) return toolFail('Repository already exists');
      const repo = forge.createRepo(
        repoNameArg,
        args.private === true,
        typeof args.description === 'string' ? args.description : '',
      );
      if (repo === null) return toolFail('Repository already exists');
      return toolOk(repoJson(repo));
    }
    case 'get_repo': {
      const missing = missingRequired(args, ['owner', 'repo']);
      if (missing !== null) return toolFail(`Missing required argument: ${missing}`);
      if (owner !== VIEWER_LOGIN) return toolFail('Not Found');
      const repo = forge.getRepo(repoName!);
      if (repo === undefined) return toolFail('Not Found');
      return toolOk(repoJson(repo));
    }
    case 'create_issue': {
      const missing = missingRequired(args, ['owner', 'repo', 'title']);
      if (missing !== null) return toolFail(`Missing required argument: ${missing}`);
      if (owner !== VIEWER_LOGIN) return toolFail('Not Found');
      const repo = forge.getRepo(repoName!);
      if (repo === undefined) return toolFail('Not Found');
      const labelsRaw = args.labels;
      const labels = Array.isArray(labelsRaw) ? labelsRaw.filter((x): x is string => typeof x === 'string') : [];
      const created = forge.createIssue(repo, args.title as string, typeof args.body === 'string' ? args.body : '', labels);
      if ('error' in created) return toolFail(created.error);
      return toolOk(issueJson(repo, created.issue));
    }
    case 'list_issues': {
      const missing = missingRequired(args, ['owner', 'repo']);
      if (missing !== null) return toolFail(`Missing required argument: ${missing}`);
      if (owner !== VIEWER_LOGIN) return toolFail('Not Found');
      const repo = forge.getRepo(repoName!);
      if (repo === undefined) return toolFail('Not Found');
      const state = typeof args.state === 'string' ? args.state : 'open';
      let list: Issue[] = [...repo.issues];
      if (state !== 'all') list = list.filter((issue) => issue.state === state);
      list.sort((a, b) => b.seq - a.seq);
      const result = { issues: list.map((issue) => issueJson(repo, issue)) };
      return toolOk(result);
    }
    case 'create_pull_request': {
      const missing = missingRequired(args, ['owner', 'repo', 'title', 'head', 'base']);
      if (missing !== null) return toolFail(`Missing required argument: ${missing}`);
      if (owner !== VIEWER_LOGIN) return toolFail('Not Found');
      const repo = forge.getRepo(repoName!);
      if (repo === undefined) return toolFail('Not Found');
      const created = forge.createPull(
        repo,
        args.head as string,
        args.base as string,
        args.title as string,
        typeof args.body === 'string' ? args.body : '',
      );
      if ('error' in created) return toolFail(created.error);
      return toolOk(pullJson(repo, created.pull, forge));
    }
    case 'merge_pull_request': {
      const missing = missingRequired(args, ['owner', 'repo', 'pull_number']);
      if (missing !== null) return toolFail(`Missing required argument: ${missing}`);
      if (owner !== VIEWER_LOGIN) return toolFail('Not Found');
      const repo = forge.getRepo(repoName!);
      if (repo === undefined) return toolFail('Not Found');
      const number = Number(pickArg(args, 'pull_number', 'pr_number', 'number'));
      const pull = repo.pulls.find((entry) => entry.number === number);
      if (pull === undefined) return toolFail('Not Found');
      const merged = forge.mergeSquash(repo, pull);
      if ('error' in merged) {
        if (merged.error === 'conflict') return toolFail('Pull request is not mergeable');
        if (merged.error === 'already-merged') return toolFail('Pull request is already merged');
        return toolFail(merged.error);
      }
      return toolOk({ merged: true, sha: merged.sha, message: `Merged pull request #${number}`, number });
    }
    default:
      return toolFail(`Unknown tool: ${name}`);
  }
}

/**
 * Handle one JSON-RPC request at /mcp. Unknown tools and unknown methods answer with
 * JSON-RPC errors (never HTTP 500); a malformed JSON body is answered by the caller.
 */
export function handleMcp(forge: Forge, body: unknown): McpResult {
  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    return rpcError(null, -32600, 'Invalid Request', 400);
  }
  const request = body as { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown };
  const id = request.id ?? null;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = request.params !== null && typeof request.params === 'object' ? (request.params as Record<string, unknown>) : {};

  if (method === 'server/discover') {
    const serverInfo = { name: SERVER_NAME, version: SERVER_VERSION };
    return rpcResult(id, {
      supportedVersions: [PROTOCOL_VERSION, '2025-06-18'],
      capabilities: { tools: {} },
      serverInfo,
      _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo },
    });
  }
  if (method === 'initialize') {
    return rpcResult(id, {
      protocolVersion: PROTOCOL_VERSION,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }
  if (method === 'notifications/initialized' || method.startsWith('notifications/')) {
    return rpcResult(id, {});
  }
  if (method === 'ping') {
    return rpcResult(id, {});
  }
  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }
  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const known = TOOLS.some((tool) => tool.name === toolName);
    if (!known) return rpcError(id, -32602, `Unknown tool: ${toolName}`);
    const result = callTool(forge, toolName, params.arguments ?? {});
    return rpcResult(id, result);
  }
  return rpcError(id, -32601, `Method not found: ${method}`);
}

// Keep the shared names referenced so the module's surface stays explicit.
export { DEFAULT_BRANCH, VIEWER_LOGIN };
export type { Issue, Pull, Repo };
