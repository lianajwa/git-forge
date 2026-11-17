// rest.ts — GitHub REST v3: repositories, branches, commits, contents, labels, issues,
// comments, pull requests and merges, plus the wire-protocol rules every reply obeys
// (application/json, a non-empty X-GitHub-Request-Id, `{message}` error bodies).
// All state lives in the shared Forge store, so REST sees what git, GraphQL and MCP wrote.

import { parseCommit } from './gitcore.ts';
import {
  DEFAULT_BRANCH,
  Forge,
  STRANGER_LOGIN,
  VIEWER_EMAIL,
  VIEWER_LOGIN,
  VIEWER_NAME,
  type Issue,
  type Pull,
  type Repo,
} from './store.ts';

export const JSON_PARSE_ERROR = Symbol('json-parse-error');

export interface RestResult {
  status: number;
  /** undefined means no body (204). */
  body?: unknown;
  /** A pre-serialized JSON body: sent as-is, never re-stringified. */
  rawBody?: string;
  headers?: Record<string, string>;
}

function ok(body: unknown, status = 200, headers?: Record<string, string>): RestResult {
  return { status, body, headers };
}

/** A 200/201 whose body is already a JSON string (the cached issue JSON). */
function okRaw(rawBody: string, status = 200, headers?: Record<string, string>): RestResult {
  return { status, rawBody, headers };
}

function err(status: number, message: string): RestResult {
  return { status, body: { message } };
}

function notFound(what = 'Not Found'): RestResult {
  return err(404, what);
}

/** A JSON object body, or null when the body is not an object. */
function bodyObject(body: unknown): Record<string, unknown> | null {
  if (body === JSON_PARSE_ERROR) return null;
  if (body !== null && typeof body === 'object' && !Array.isArray(body)) return body as Record<string, unknown>;
  return null;
}

function bodyString(body: Record<string, unknown> | null, key: string): string | undefined {
  const value = body?.[key];
  return typeof value === 'string' ? value : undefined;
}

function bodyBoolean(body: Record<string, unknown> | null, key: string): boolean | undefined {
  const value = body?.[key];
  return typeof value === 'boolean' ? value : undefined;
}

// ---------------------------------------------------------------------------------------------
// JSON views
// ---------------------------------------------------------------------------------------------

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.databaseId,
    node_id: `R_${repo.databaseId}`,
    name: repo.name,
    full_name: `${VIEWER_LOGIN}/${repo.name}`,
    owner: { login: VIEWER_LOGIN, id: 1000, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: DEFAULT_BRANCH,
    created_at: repo.createdAt,
    updated_at: repo.createdAt,
    has_issues: true,
    has_pull_requests: true,
    url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}`,
    html_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}`,
  };
}

export function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I_${issue.id}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ id: repo.labels.get(name)?.id ?? 0, name, color: repo.labels.get(name)?.color ?? 'ededed' })),
    user: { login: VIEWER_LOGIN, id: 1000, type: 'User' },
    created_at: issue.createdAt,
    updated_at: issue.createdAt,
    url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/issues/${issue.number}`,
    html_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/issues/${issue.number}`,
  };
}

/**
 * The serialized issue JSON, cached on the repo. An issue's JSON is a pure function of
 * the issue and its labels, and labels never change after creation while an issue's
 * title, body and state change only through the PATCH route (and GraphQL's closeIssue),
 * which both drop the cached entry — so a hit is byte-identical to a fresh build.
 */
export function issueJsonText(repo: Repo, issue: Issue): string {
  const cached = repo.issueJsonCache.get(issue.number);
  if (cached !== undefined) return cached;
  const built = JSON.stringify(issueJson(repo, issue));
  repo.issueJsonCache.set(issue.number, built);
  return built;
}

/** Drop one issue's cached JSON after a mutation (PATCH, GraphQL closeIssue). */
export function dropIssueJsonCache(repo: Repo, issueNumber: number): void {
  repo.issueJsonCache.delete(issueNumber);
}

export function pullJson(repo: Repo, pull: Pull, forge: Forge): Record<string, unknown> {
  const headSha = forge.branchTip(repo, pull.head) ?? '';
  const mergeable = !forge.pullConflict(repo, pull);
  return {
    id: pull.id,
    node_id: `PR_${pull.id}`,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: { ref: pull.head, sha: headSha, label: `${VIEWER_LOGIN}:${pull.head}` },
    base: { ref: pull.base, sha: pull.baseSha, label: `${VIEWER_LOGIN}:${pull.base}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeSha,
    mergeable,
    user: { login: VIEWER_LOGIN, id: 1000, type: 'User' },
    created_at: pull.createdAt,
    updated_at: pull.createdAt,
    url: `/api/v3/repos/${VIEWER_LOGIN}/${repo.name}/pulls/${pull.number}`,
    html_url: `https://github.com/${VIEWER_LOGIN}/${repo.name}/pull/${pull.number}`,
  };
}

/** Parse a "Name <email> 1700000000 +0000" identity line into its parts. */
function parseIdentity(line: string): { name: string; email: string; date: string } {
  const match = /^(.*?) <(.*?)> (\d+) ([+-]\d{4})$/.exec(line);
  if (!match) return { name: line, email: '', date: '' };
  const epoch = Number(match[3]);
  return { name: match[1] ?? '', email: match[2] ?? '', date: new Date(epoch * 1000).toISOString() };
}

// ---------------------------------------------------------------------------------------------
// pagination
// ---------------------------------------------------------------------------------------------

interface Page {
  perPage: number;
  page: number;
}

function pageOf(query: URLSearchParams): Page {
  const perPageRaw = Number(query.get('per_page') ?? '30');
  const pageRaw = Number(query.get('page') ?? '1');
  const perPage = Number.isFinite(perPageRaw) ? Math.min(100, Math.max(1, Math.trunc(perPageRaw))) : 30;
  const page = Number.isFinite(pageRaw) ? Math.max(1, Math.trunc(pageRaw)) : 1;
  return { perPage, page };
}

function paginate<T>(items: readonly T[], page: Page): { slice: T[]; totalPages: number } {
  const totalPages = Math.max(1, Math.ceil(items.length / page.perPage));
  const start = (page.page - 1) * page.perPage;
  return { slice: items.slice(start, start + page.perPage), totalPages };
}

/** A GitHub `Link` header for the next/last pages, absolute on this origin. */
function linkHeader(host: string, path: string, query: URLSearchParams, page: Page, totalPages: number): string | null {
  const build = (pageNumber: number): string => {
    const next = new URLSearchParams(query);
    next.set('page', String(pageNumber));
    next.set('per_page', String(page.perPage));
    return `http://${host}${path}?${next.toString()}`;
  };
  const links: string[] = [];
  if (page.page < totalPages) links.push(`<${build(page.page + 1)}>; rel="next"`);
  if (totalPages > 1) links.push(`<${build(totalPages)}>; rel="last"`);
  if (page.page > 1) links.push(`<${build(1)}>; rel="first"`);
  if (page.page > 1) links.push(`<${build(page.page - 1)}>; rel="prev"`);
  return links.length > 0 ? links.join(', ') : null;
}

// ---------------------------------------------------------------------------------------------
// the router
// ---------------------------------------------------------------------------------------------

/**
 * Handle one REST call. `segments` are the decoded path parts after /api/v3; `query` holds
 * the query string; `body` is the parsed JSON body (or JSON_PARSE_ERROR, or undefined
 * when there was no body). Returns the status, body and any extra headers.
 */
export function handleRest(
  forge: Forge,
  method: string,
  segments: string[],
  query: URLSearchParams,
  body: unknown,
  host: string,
): RestResult {
  if (body === JSON_PARSE_ERROR) return err(400, 'Invalid JSON');
  const path = `/api/v3${segments.length > 0 ? `/${segments.join('/')}` : ''}`;

  // API root and meta.
  if (segments.length === 0) {
    if (method === 'GET') {
      return ok(
        {
          current_user_url: '/api/v3/user',
          rate_limit_url: '/api/v3/rate_limit',
          repository_url: '/api/v3/repos/{owner}/{repo}',
        },
        200,
        { 'x-oauth-scopes': 'repo, read:org' },
      );
    }
    return notFound();
  }
  if (segments.length === 1 && segments[0] === 'meta' && method === 'GET') {
    return ok({ installed_version: 'forge 1.0.0' });
  }

  // The token's own user.
  if (segments.length === 1 && segments[0] === 'user' && method === 'GET') {
    return ok({ login: VIEWER_LOGIN, id: 1000, name: VIEWER_NAME, email: VIEWER_EMAIL, type: 'User' });
  }

  // /user/repos: list and create.
  if (segments.length === 2 && segments[0] === 'user' && segments[1] === 'repos') {
    if (method === 'GET') {
      const page = pageOf(query);
      const all = [...forge.repos.values()].sort((a, b) => a.seq - b.seq);
      const { slice, totalPages } = paginate(all, page);
      const headers: Record<string, string> = {};
      const link = linkHeader(host, path, query, page, totalPages);
      if (link !== null) headers.link = link;
      return ok(slice.map(repoJson), 200, headers);
    }
    if (method === 'POST') {
      const input = bodyObject(body);
      const name = bodyString(input, 'name');
      if (name === undefined || name === '') return err(422, 'name is required');
      if (name.includes('/')) return err(422, 'invalid repository name');
      if (forge.getRepo(name) !== undefined) return err(422, 'Repository already exists');
      const repo = forge.createRepo(name, bodyBoolean(input, 'private') ?? false, bodyString(input, 'description') ?? '');
      if (repo === null) return err(422, 'Repository already exists');
      return ok(repoJson(repo), 201);
    }
    return notFound();
  }

  // /repos/{owner}/{name}[/...]: everything else lives under a repository.
  if (segments.length >= 3 && segments[0] === 'repos') {
    const owner = segments[1]!;
    const name = segments[2]!;
    if (owner !== VIEWER_LOGIN) return notFound('Not Found');
    const repo = forge.getRepo(name);
    if (repo === undefined) return notFound('Not Found');
    return handleRepo(forge, repo, method, segments.slice(3), query, body, host, path);
  }

  return notFound();
}

// ---------------------------------------------------------------------------------------------
// repository-scoped routes
// ---------------------------------------------------------------------------------------------

function handleRepo(
  forge: Forge,
  repo: Repo,
  method: string,
  tail: string[],
  query: URLSearchParams,
  body: unknown,
  host: string,
  path: string,
): RestResult {
  // The repository itself.
  if (tail.length === 0) {
    if (method === 'GET') return ok(repoJson(repo));
    if (method === 'PATCH') {
      const input = bodyObject(body);
      const description = bodyString(input, 'description');
      if (description !== undefined) repo.description = description;
      const isPrivate = bodyBoolean(input, 'private');
      if (isPrivate !== undefined) repo.isPrivate = isPrivate;
      return ok(repoJson(repo));
    }
    if (method === 'DELETE') {
      forge.deleteRepo(repo.name);
      return { status: 204 };
    }
    return notFound();
  }

  // Branches.
  if (tail[0] === 'branches' && tail.length === 1 && method === 'GET') {
    const branches = [...repo.refs.keys()]
      .filter((ref) => ref.startsWith('refs/heads/'))
      .map((ref) => ref.slice('refs/heads/'.length))
      .sort()
      .map((name) => ({ name, commit: { sha: repo.refs.get(`refs/heads/${name}`) }, protected: false }));
    return ok(branches);
  }
  if (tail[0] === 'branches' && tail.length === 2 && method === 'GET') {
    const tip = forge.branchTip(repo, tail[1]!);
    if (tip === undefined) return notFound('Branch not found');
    return ok({ name: tail[1], commit: { sha: tip }, protected: false });
  }

  // Commits: ?sha=<branch or commit>; newest first.
  if (tail[0] === 'commits' && tail.length === 1 && method === 'GET') {
    const shaParam = query.get('sha');
    let tip: string | undefined;
    if (shaParam === null) tip = forge.branchTip(repo, DEFAULT_BRANCH);
    else if (/^[0-9a-f]{40}$/.test(shaParam)) tip = repo.objects.has(shaParam) ? shaParam : undefined;
    else tip = forge.branchTip(repo, shaParam);
    if (tip === undefined) return notFound('No commit found for SHA');
    const page = pageOf(query);
    const walk = forge.listCommits(repo, shaParam ?? undefined);
    const { slice, totalPages } = paginate(walk, page);
    const headers: Record<string, string> = {};
    const link = linkHeader(host, path, query, page, totalPages);
    if (link !== null) headers.link = link;
    const entries = slice.map((oid) => {
      const object = repo.objects.get(oid)!;
      const parsed = parseCommit(object.bytes);
      const author = parseIdentity(parsed.author);
      const committer = parseIdentity(parsed.committer);
      return {
        sha: oid,
        commit: {
          message: parsed.message,
          author: { name: author.name, email: author.email, date: author.date },
          committer: { name: committer.name, email: committer.email, date: committer.date },
        },
        author: { login: VIEWER_LOGIN, id: 1000, type: 'User' },
        parents: parsed.parents.map((parent) => ({ sha: parent })),
      };
    });
    return ok(entries, 200, headers);
  }

  // git refs: create a ref, or read one branch ref.
  if (tail[0] === 'git' && tail[1] === 'refs' && tail.length === 2 && method === 'POST') {
    const input = bodyObject(body);
    const ref = bodyString(input, 'ref');
    const sha = bodyString(input, 'sha');
    if (ref === undefined || sha === undefined) return err(422, 'ref and sha are required');
    if (!ref.startsWith('refs/heads/') && !ref.startsWith('refs/tags/')) return err(422, 'invalid ref');
    if (repo.refs.has(ref)) return err(422, 'Reference already exists');
    const target = repo.objects.get(sha);
    if (!target || !/^[0-9a-f]{40}$/.test(sha)) return err(422, 'Invalid SHA');
    repo.refs.set(ref, sha);
    forge.touchGit(repo);
    return ok({ ref, object: { sha, type: target.kind } }, 201);
  }
  if (tail[0] === 'git' && tail[1] === 'refs' && tail[2] === 'heads' && tail.length === 4 && method === 'GET') {
    const tip = forge.branchTip(repo, tail[3]!);
    if (tip === undefined) return notFound('Branch not found');
    return ok({ ref: `refs/heads/${tail[3]}`, object: { sha: tip, type: 'commit' } });
  }

  // contents: files and directories, base64 through PUT.
  if (tail[0] === 'contents') {
    const contentPath = tail.slice(1).join('/');
    const ref = query.get('ref') ?? undefined;
    if (method === 'GET') {
      const read = forge.readPath(repo, contentPath, ref);
      if (read === null) return notFound('Not Found');
      if (read.kind === 'blob') {
        const bytes = read.bytes ?? Buffer.alloc(0);
        return ok({
          type: 'file',
          encoding: 'base64',
          content: bytes.toString('base64'),
          sha: read.oid,
          size: bytes.length,
          name: contentPath.split('/').pop() ?? contentPath,
          path: contentPath,
        });
      }
      const dirEntries = forge.listDir(repo, contentPath, ref);
      if (dirEntries === null) return notFound('Not Found');
      const entries = dirEntries.map((entry) => ({
        type: entry.kind,
        name: entry.name,
        path: entry.path,
        sha: entry.oid,
        size: entry.kind === 'file' ? (repo.objects.get(entry.oid)?.bytes.length ?? 0) : 0,
      }));
      return ok(entries);
    }
    if (method === 'PUT') {
      const input = bodyObject(body);
      const content = bodyString(input, 'content');
      const message = bodyString(input, 'message') ?? 'Update file';
      const branch = bodyString(input, 'branch');
      const sha = bodyString(input, 'sha');
      if (content === undefined) return err(422, 'content is required');
      let bytes: Buffer;
      try {
        bytes = Buffer.from(content.replace(/\s+/g, ''), 'base64');
      } catch {
        return err(422, 'content is not valid base64');
      }
      const result = forge.writeFile(repo, contentPath, bytes, message, branch, sha);
      if (result.error !== undefined) {
        if (result.error === 'stale sha') return err(409, 'sha does not match');
        if (result.error === 'file does not exist') return err(404, 'Not Found');
        return err(422, result.error);
      }
      const fileName = contentPath.split('/').pop() ?? contentPath;
      return ok(
        {
          content: { name: fileName, path: contentPath, sha: result.blobSha, size: bytes.length, type: 'file', encoding: 'base64' },
          commit: { sha: result.commitSha, message },
        },
        result.created ? 201 : 200,
      );
    }
    if (method === 'DELETE') {
      const input = bodyObject(body);
      const message = bodyString(input, 'message') ?? 'Delete file';
      const branch = bodyString(input, 'branch');
      const sha = bodyString(input, 'sha');
      const result = forge.writeFile(repo, contentPath, null, message, branch, sha);
      if (result.error !== undefined) {
        if (result.error === 'stale sha') return err(409, 'sha does not match');
        if (result.error === 'file does not exist') return err(404, 'Not Found');
        return err(422, result.error);
      }
      return ok({ commit: { sha: result.commitSha, message } });
    }
    return notFound();
  }

  // labels.
  if (tail[0] === 'labels' && tail.length === 1) {
    if (method === 'GET') {
      return ok([...repo.labels.values()].map((label) => ({ id: label.id, name: label.name, color: label.color, description: '' })));
    }
    if (method === 'POST') {
      const input = bodyObject(body);
      const name = bodyString(input, 'name');
      if (name === undefined || name === '') return err(422, 'name is required');
      const color = bodyString(input, 'color') ?? 'ededed';
      const existing = repo.labels.get(name);
      if (existing) return ok({ id: existing.id, name: existing.name, color: existing.color, description: '' }, 201);
      const id = repo.nextLabelId++;
      repo.labels.set(name, { id, name, color });
      return ok({ id, name, color, description: '' }, 201);
    }
    return notFound();
  }

  // issues and comments.
  if (tail[0] === 'issues' && tail.length === 1) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      const direction = query.get('direction') ?? 'desc';
      let list = [...repo.issues];
      if (state !== 'all') list = list.filter((issue) => issue.state === state);
      list.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
      const page = pageOf(query);
      const { slice, totalPages } = paginate(list, page);
      const headers: Record<string, string> = {};
      const link = linkHeader(host, path, query, page, totalPages);
      if (link !== null) headers.link = link;
      return ok(slice.map((issue) => issueJson(repo, issue)), 200, headers);
    }
    if (method === 'POST') {
      const input = bodyObject(body);
      const title = bodyString(input, 'title');
      if (title === undefined || title === '') return err(422, 'title is required');
      const bodyText = bodyString(input, 'body') ?? '';
      const labelsRaw = input?.labels;
      const labels = Array.isArray(labelsRaw) ? labelsRaw.filter((x): x is string => typeof x === 'string') : [];
      const created = forge.createIssue(repo, title, bodyText, labels);
      if ('error' in created) return err(422, created.error);
      return ok(issueJson(repo, created.issue), 201);
    }
    return notFound();
  }
  if (tail[0] === 'issues' && tail.length === 2) {
    const number = Number(tail[1]);
    const issue = repo.issueByNumber.get(number) ?? repo.issues.find((entry) => entry.number === number);
    if (issue === undefined) return notFound('Not Found');
    if (method === 'GET') return okRaw(issueJsonText(repo, issue));
    if (method === 'PATCH') {
      const input = bodyObject(body);
      const state = bodyString(input, 'state');
      if (state === 'open' || state === 'closed') issue.state = state;
      const title = bodyString(input, 'title');
      if (title !== undefined) issue.title = title;
      const bodyText = bodyString(input, 'body');
      if (bodyText !== undefined) issue.body = bodyText;
      dropIssueJsonCache(repo, issue.number);
      return okRaw(issueJsonText(repo, issue));
    }
    return notFound();
  }
  if (tail[0] === 'issues' && tail.length === 3 && tail[2] === 'comments') {
    const number = Number(tail[1]);
    const issue = repo.issues.find((entry) => entry.number === number);
    if (issue === undefined) return notFound('Not Found');
    if (method === 'GET') {
      const comments = (repo.comments.get(number) ?? []).map((comment) => ({
        id: comment.id,
        body: comment.body,
        user: { login: VIEWER_LOGIN, id: 1000, type: 'User' },
        created_at: issue.createdAt,
      }));
      return ok(comments);
    }
    if (method === 'POST') {
      const input = bodyObject(body);
      const text = bodyString(input, 'body');
      if (text === undefined || text === '') return err(422, 'body is required');
      const comment = forge.addComment(repo, number, text);
      if (comment === null) return notFound('Not Found');
      return ok(
        {
          id: comment.id,
          body: comment.body,
          user: { login: VIEWER_LOGIN, id: 1000, type: 'User' },
          created_at: issue.createdAt,
        },
        201,
      );
    }
    return notFound();
  }

  // pull requests.
  if (tail[0] === 'pulls' && tail.length === 1) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      let list = [...repo.pulls];
      if (state !== 'all') list = list.filter((pull) => pull.state === state);
      list.sort((a, b) => b.seq - a.seq);
      const page = pageOf(query);
      const { slice, totalPages } = paginate(list, page);
      const headers: Record<string, string> = {};
      const link = linkHeader(host, path, query, page, totalPages);
      if (link !== null) headers.link = link;
      return ok(slice.map((pull) => pullJson(repo, pull, forge)), 200, headers);
    }
    if (method === 'POST') {
      const input = bodyObject(body);
      const head = bodyString(input, 'head');
      const base = bodyString(input, 'base') ?? DEFAULT_BRANCH;
      const title = bodyString(input, 'title');
      if (head === undefined || title === undefined || title === '') return err(422, 'head and title are required');
      const bodyText = bodyString(input, 'body') ?? '';
      const created = forge.createPull(repo, head, base, title, bodyText);
      if ('error' in created) return err(422, created.error);
      return ok(pullJson(repo, created.pull, forge), 201);
    }
    return notFound();
  }
  if (tail[0] === 'pulls' && tail.length === 2) {
    const number = Number(tail[1]);
    const pull = repo.pulls.find((entry) => entry.number === number);
    if (pull === undefined) return notFound('Not Found');
    if (method === 'GET') return ok(pullJson(repo, pull, forge));
    return notFound();
  }
  if (tail[0] === 'pulls' && tail.length === 3 && tail[2] === 'files' && method === 'GET') {
    const number = Number(tail[1]);
    const pull = repo.pulls.find((entry) => entry.number === number);
    if (pull === undefined) return notFound('Not Found');
    const files = forge.pullFiles(repo, pull).map((file) => ({
      filename: file.path,
      status: file.status === 'removed' ? 'removed' : file.status === 'added' ? 'added' : 'modified',
      additions: file.additions,
      deletions: file.deletions,
      changes: file.changes,
    }));
    return ok(files);
  }
  if (tail[0] === 'pulls' && tail.length === 3 && tail[2] === 'merge' && method === 'PUT') {
    const number = Number(tail[1]);
    const pull = repo.pulls.find((entry) => entry.number === number);
    if (pull === undefined) return notFound('Not Found');
    const merged = forge.mergeSquash(repo, pull);
    if ('error' in merged) {
      if (merged.error === 'conflict') return err(405, 'Pull request is not mergeable');
      if (merged.error === 'already-merged') return err(405, 'Pull request is already merged');
      return err(422, merged.error);
    }
    return ok({ merged: true, sha: merged.sha, message: `Merged pull request #${number}` });
  }

  return notFound();
}

// Re-export the stranger login for callers that resolve owners.
export { STRANGER_LOGIN };
