// graphql.ts — a small, purpose-built GraphQL executor for the forge's /api/graphql.
// It parses the 18 documents the conformance suite sends (queries, mutations, aliases,
// fragments, variables, batch arrays, and __type introspection) and resolves them against
// the same Forge store as REST, git and MCP. Unknown fields fail the way GraphQL does:
// HTTP 200 with an errors array naming the field.

import { DEFAULT_BRANCH, Forge, VIEWER_LOGIN, type Issue, type Pull, type Repo } from './store.ts';
import { dropIssueJsonCache } from './rest.ts';

// ---------------------------------------------------------------------------------------------
// lexer + parser (a compact recursive-descent parser for the GraphQL document subset we serve)
// ---------------------------------------------------------------------------------------------

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'punct'; value: string }
  | { kind: 'eof' };

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let at = 0;
  while (at < source.length) {
    const ch = source[at]!;
    if (/\s/.test(ch) || ch === ',') {
      at++;
      continue;
    }
    if (ch === '#') {
      while (at < source.length && source[at] !== '\n') at++;
      continue;
    }
    if (ch === '"') {
      let value = '';
      at++;
      while (at < source.length && source[at] !== '"') {
        if (source[at] === '\\' && at + 1 < source.length) {
          const escaped = source[at + 1]!;
          value += escaped === 'n' ? '\n' : escaped === 't' ? '\t' : escaped === 'r' ? '\r' : escaped;
          at += 2;
        } else {
          value += source[at];
          at++;
        }
      }
      at++;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let text = '';
      while (at < source.length && /[0-9]/.test(source[at]!)) {
        text += source[at];
        at++;
      }
      tokens.push({ kind: 'int', value: Number(text) });
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let text = '';
      while (at < source.length && /[A-Za-z0-9_]/.test(source[at]!)) {
        text += source[at];
        at++;
      }
      tokens.push({ kind: 'name', value: text });
      continue;
    }
    if ('{}()[]:!=$@&|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      at++;
      continue;
    }
    if (ch === '.' && source.slice(at, at + 3) === '...') {
      tokens.push({ kind: 'punct', value: '...' });
      at += 3;
      continue;
    }
    throw new Error(`unexpected character ${ch}`);
  }
  tokens.push({ kind: 'eof' });
  return tokens;
}

export type GqlValue =
  | string
  | number
  | boolean
  | null
  | { [key: string]: GqlValue }
  | GqlValue[]
  | { __variable: string }
  | { __enum: string };

export interface Selection {
  name: string;
  alias: string | null;
  args: { [key: string]: GqlValue };
  children: Selection[];
  fragment: string | null;
  inlineType: string | null;
}

export interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: { [name: string]: GqlValue };
  selections: Selection[];
}

class Parser {
  at = 0;

  tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  peek(): Token {
    return this.tokens[this.at]!;
  }

  next(): Token {
    const token = this.tokens[this.at]!;
    this.at++;
    return token;
  }

  expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected ${value}`);
  }

  skipPunct(value: string): boolean {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === value) {
      this.at++;
      return true;
    }
    return false;
  }

  parseDocument(): { operations: Operation[]; fragments: { [name: string]: Selection[] } } {
    const operations: Operation[] = [];
    const fragments: { [name: string]: Selection[] } = {};
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const nameToken = this.next();
        const name = nameToken.kind === 'name' ? nameToken.value : '';
        // 'on Type'
        this.next();
        this.next();
        fragments[name] = this.parseSelectionSet();
        continue;
      }
      let kind: 'query' | 'mutation' = 'query';
      let name: string | null = null;
      if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
        kind = token.value;
        this.next();
        const maybeName = this.peek();
        if (maybeName.kind === 'name') {
          name = maybeName.value;
          this.next();
        }
      } else if (token.kind === 'name' && token.value === 'subscription') {
        throw new Error('subscriptions are not supported');
      }
      const variables = this.parseVariableDefinitions();
      const selections = this.parseSelectionSet();
      operations.push({ kind, name, variables, selections });
    }
    return { operations, fragments };
  }

  parseVariableDefinitions(): { [name: string]: GqlValue } {
    const variables: { [name: string]: GqlValue } = {};
    if (!this.skipPunct('(')) return variables;
    while (!this.skipPunct(')')) {
      if (this.peek().kind === 'eof') break;
      this.expectPunct('$');
      const nameToken = this.next();
      if (nameToken.kind !== 'name') break;
      this.expectPunct(':');
      // Skip the type (a name, possibly wrapped in [ ] and !).
      while (this.peek().kind === 'name' || (this.peek().kind === 'punct' && ['[', ']', '!'].includes((this.peek() as { value: string }).value))) {
        this.next();
      }
      if (this.skipPunct('=')) this.parseValue();
      variables[nameToken.value] = null;
    }
    return variables;
  }

  parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (!this.skipPunct('}')) {
      if (this.peek().kind === 'eof') throw new Error('unterminated selection set');
      selections.push(this.parseSelection());
    }
    return selections;
  }

  parseSelection(): Selection {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '...') {
      this.next();
      const next = this.next();
      if (next.kind === 'name' && next.value === 'on') {
        const typeToken = this.next();
        const inlineType = typeToken.kind === 'name' ? typeToken.value : null;
        return { name: '', alias: null, args: {}, children: this.parseSelectionSet(), fragment: null, inlineType };
      }
      if (next.kind === 'name') {
        return { name: '', alias: null, args: {}, children: [], fragment: next.value, inlineType: null };
      }
      throw new Error('bad fragment spread');
    }
    const nameToken = this.next();
    if (nameToken.kind !== 'name') throw new Error('expected field name');
    let name = nameToken.value;
    let alias: string | null = null;
    if (this.skipPunct(':')) {
      alias = name;
      const real = this.next();
      if (real.kind !== 'name') throw new Error('expected field name after alias');
      name = real.value;
    }
    const args: { [key: string]: GqlValue } = {};
    if (this.skipPunct('(')) {
      while (!this.skipPunct(')')) {
        if (this.peek().kind === 'eof') throw new Error('unterminated arguments');
        const argToken = this.next();
        if (argToken.kind !== 'name') throw new Error('expected argument name');
        this.expectPunct(':');
        args[argToken.value] = this.parseValue();
      }
    }
    // Directives (@include/@skip) are ignored: the suite never sends them.
    while (this.peek().kind === 'punct' && (this.peek() as { value: string }).value === '@') {
      this.next();
      this.next();
      if (this.skipPunct('(')) {
        while (!this.skipPunct(')')) this.parseValue();
      }
    }
    const children = this.peek().kind === 'punct' && (this.peek() as { value: string }).value === '{' ? this.parseSelectionSet() : [];
    return { name, alias, args, children, fragment: null, inlineType: null };
  }

  parseValue(): GqlValue {
    const token = this.next();
    if (token.kind === 'string') return token.value;
    if (token.kind === 'int') return token.value;
    if (token.kind === 'name') {
      if (token.value === 'true') return true;
      if (token.value === 'false') return false;
      if (token.value === 'null') return null;
      return { __enum: token.value };
    }
    if (token.kind === 'punct' && token.value === '$') {
      const nameToken = this.next();
      if (nameToken.kind !== 'name') throw new Error('expected variable name');
      return { __variable: nameToken.value };
    }
    if (token.kind === 'punct' && token.value === '[') {
      const items: GqlValue[] = [];
      while (!this.skipPunct(']')) items.push(this.parseValue());
      return items;
    }
    if (token.kind === 'punct' && token.value === '{') {
      const object: { [key: string]: GqlValue } = {};
      while (!this.skipPunct('}')) {
        const keyToken = this.next();
        if (keyToken.kind !== 'name') throw new Error('expected object key');
        this.expectPunct(':');
        object[keyToken.value] = this.parseValue();
      }
      return object;
    }
    throw new Error('unexpected value');
  }
}

/** Substitute $variables into a parsed value, then unwrap enums to their names. */
function resolveValue(value: GqlValue, variables: { [key: string]: unknown }): unknown {
  if (value === null || typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') return value;
  if (Array.isArray(value)) return value.map((entry) => resolveValue(entry, variables));
  if (typeof value === 'object') {
    if ('__variable' in value) return variables[value.__variable] ?? null;
    if ('__enum' in value) return value.__enum;
    const out: { [key: string]: unknown } = {};
    for (const [key, entry] of Object.entries(value)) out[key] = resolveValue(entry as GqlValue, variables);
    return out;
  }
  return value;
}

// ---------------------------------------------------------------------------------------------
// schema tables (used for validation and __type introspection)
// ---------------------------------------------------------------------------------------------

const FIELD_TYPES: { [type: string]: { [field: string]: string } } = {
  Query: { viewer: 'User', repository: 'Repository', node: 'Node', __type: '__Type' },
  Mutation: {
    createIssue: 'CreateIssuePayload',
    closeIssue: 'CloseIssuePayload',
    createPullRequest: 'CreatePullRequestPayload',
    mergePullRequest: 'MergePullRequestPayload',
    addComment: 'AddCommentPayload',
  },
  User: { id: 'ID', login: 'String', name: 'String', email: 'String' },
  Repository: {
    id: 'ID',
    databaseId: 'Int',
    name: 'String',
    nameWithOwner: 'String',
    owner: 'User',
    defaultBranchRef: 'Ref',
    viewerPermission: 'String',
    hasIssuesEnabled: 'Boolean',
    issue: 'Issue',
    issues: 'IssueConnection',
    pullRequests: 'PullRequestConnection',
    pullRequest: 'PullRequest',
  },
  Issue: { id: 'ID', number: 'Int', title: 'String', state: 'String', body: 'String', repository: 'Repository' },
  PullRequest: {
    id: 'ID',
    number: 'Int',
    title: 'String',
    body: 'String',
    state: 'String',
    merged: 'Boolean',
    headRefName: 'String',
    baseRefName: 'String',
    mergeable: 'String',
    files: 'PullRequestFileConnection',
  },
  IssueConnection: { nodes: '[Issue]', pageInfo: 'PageInfo', totalCount: 'Int' },
  PullRequestConnection: { nodes: '[PullRequest]', pageInfo: 'PageInfo', totalCount: 'Int' },
  PullRequestFileConnection: { nodes: '[PullRequestFile]', totalCount: 'Int' },
  PullRequestFile: { path: 'String' },
  PageInfo: { hasNextPage: 'Boolean', endCursor: 'String', hasPreviousPage: 'Boolean', startCursor: 'String' },
  Ref: { name: 'String' },
  __Type: { name: 'String', kind: 'String', fields: '[__Field]', enumValues: '[__EnumValue]' },
  __Field: { name: 'String', description: 'String' },
  __EnumValue: { name: 'String', description: 'String' },
  CreateIssuePayload: { issue: 'Issue' },
  CloseIssuePayload: { issue: 'Issue' },
  CreatePullRequestPayload: { pullRequest: 'PullRequest' },
  MergePullRequestPayload: { pullRequest: 'PullRequest' },
  AddCommentPayload: { commentEdge: 'IssueCommentEdge' },
  IssueCommentEdge: { node: 'IssueComment' },
  IssueComment: { id: 'ID', body: 'String' },
};

const SCALAR_TYPES = new Set(['ID', 'String', 'Int', 'Boolean']);
const TYPE_KIND: { [type: string]: string } = {
  Query: 'OBJECT',
  Mutation: 'OBJECT',
  User: 'OBJECT',
  Repository: 'OBJECT',
  Issue: 'OBJECT',
  PullRequest: 'OBJECT',
  IssueConnection: 'OBJECT',
  PullRequestConnection: 'OBJECT',
  PullRequestFileConnection: 'OBJECT',
  PullRequestFile: 'OBJECT',
  PageInfo: 'OBJECT',
  Ref: 'OBJECT',
  __Type: 'OBJECT',
  __Field: 'OBJECT',
  __EnumValue: 'OBJECT',
  IssueCommentEdge: 'OBJECT',
  IssueComment: 'OBJECT',
  CreateIssuePayload: 'OBJECT',
  CloseIssuePayload: 'OBJECT',
  CreatePullRequestPayload: 'OBJECT',
  MergePullRequestPayload: 'OBJECT',
  AddCommentPayload: 'OBJECT',
  IssueState: 'ENUM',
  PullRequestState: 'ENUM',
  MergeableState: 'ENUM',
  SearchType: 'ENUM',
};

const ENUM_VALUES: { [type: string]: string[] } = {
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  SearchType: ['ISSUE', 'REPOSITORY', 'USER'],
};

/** An opaque, stable global id: base64 of a typed key. */
export function globalId(kind: string, key: string): string {
  return Buffer.from(`${kind}:${key}`, 'utf8').toString('base64');
}

function parseGlobalId(id: string): { kind: string; key: string } | null {
  try {
    const decoded = Buffer.from(id, 'base64').toString('utf8');
    const colon = decoded.indexOf(':');
    if (colon < 0) return null;
    return { kind: decoded.slice(0, colon), key: decoded.slice(colon + 1) };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------------------------
// internal value wrappers (never leak into JSON output)
// ---------------------------------------------------------------------------------------------

type Value =
  | { __kind: 'viewer' }
  | { __kind: 'repo'; repo: Repo }
  | { __kind: 'issue'; repo: Repo; issue: Issue }
  | { __kind: 'pull'; repo: Repo; pull: Pull }
  | { __kind: 'issuesConn'; repo: Repo; issues: Issue[]; hasNext: boolean; endCursor: string | null }
  | { __kind: 'pullsConn'; repo: Repo; pulls: Pull[]; hasNext: boolean; endCursor: string | null }
  | { __kind: 'filesConn'; files: { path: string }[] }
  | { __kind: 'pageInfo'; hasNext: boolean; endCursor: string | null }
  | { __kind: 'ref'; name: string }
  | { __kind: 'gqlType'; name: string }
  | { __kind: 'comment'; id: number; body: string }
  | { __kind: 'payload'; fields: { [key: string]: Value | null } }
  | null;

function valueType(value: Value): string {
  if (value === null) return 'Node';
  switch (value.__kind) {
    case 'viewer':
      return 'User';
    case 'repo':
      return 'Repository';
    case 'issue':
      return 'Issue';
    case 'pull':
      return 'PullRequest';
    case 'issuesConn':
      return 'IssueConnection';
    case 'pullsConn':
      return 'PullRequestConnection';
    case 'filesConn':
      return 'PullRequestFileConnection';
    case 'pageInfo':
      return 'PageInfo';
    case 'ref':
      return 'Ref';
    case 'gqlType':
      return '__Type';
    case 'comment':
      return 'IssueComment';
    case 'payload':
      return 'Payload';
  }
}

function cursorFor(seq: number): string {
  return Buffer.from(`cursor:${seq}`, 'utf8').toString('base64');
}

function seqFromCursor(cursor: string | null): number | null {
  if (cursor === null) return null;
  try {
    const decoded = Buffer.from(cursor, 'base64').toString('utf8');
    if (!decoded.startsWith('cursor:')) return null;
    const seq = Number(decoded.slice('cursor:'.length));
    return Number.isFinite(seq) ? seq : null;
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------------------------
// validation: unknown fields fail the whole document the way GraphQL validation does
// ---------------------------------------------------------------------------------------------

function validateSelections(
  typeName: string,
  selections: readonly Selection[],
  fragments: { [name: string]: Selection[] },
  errors: string[],
): void {
  const fields = FIELD_TYPES[typeName];
  if (fields === undefined) return;
  for (const selection of selections) {
    if (selection.fragment !== null) {
      const fragment = fragments[selection.fragment];
      if (fragment) validateSelections(typeName, fragment, fragments, errors);
      continue;
    }
    if (selection.inlineType !== null) {
      validateSelections(selection.inlineType, selection.children, fragments, errors);
      continue;
    }
    if (selection.name === '__typename') continue;
    const fieldType = fields[selection.name];
    if (fieldType === undefined) {
      errors.push(`Cannot query field "${selection.name}" on type "${typeName}".`);
      continue;
    }
    const inner = fieldType.replace(/[\[\]]/g, '');
    if (!SCALAR_TYPES.has(inner) && selection.children.length > 0) {
      validateSelections(inner, selection.children, fragments, errors);
    }
  }
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

interface ExecCtx {
  forge: Forge;
  variables: { [key: string]: unknown };
  fragments: { [name: string]: Selection[] };
}

function argValue(selection: Selection, name: string, ctx: ExecCtx): unknown {
  const raw = selection.args[name];
  if (raw === undefined) return undefined;
  return resolveValue(raw, ctx.variables);
}

function executeSelections(ctx: ExecCtx, typeName: string, value: Value, selections: readonly Selection[]): { [key: string]: unknown } {
  const out: { [key: string]: unknown } = {};
  for (const selection of selections) {
    if (selection.fragment !== null) {
      const fragment = ctx.fragments[selection.fragment];
      if (fragment) Object.assign(out, executeSelections(ctx, typeName, value, fragment));
      continue;
    }
    if (selection.inlineType !== null) {
      if (selection.inlineType === typeName || typeName === 'Node') {
        Object.assign(out, executeSelections(ctx, selection.inlineType, value, selection.children));
      }
      continue;
    }
    const key = selection.alias ?? selection.name;
    if (selection.name === '__typename') {
      out[key] = typeName;
      continue;
    }
    out[key] = executeField(ctx, typeName, value, selection);
  }
  return out;
}

function resolveObject(ctx: ExecCtx, typeName: string, value: Value, children: readonly Selection[]): unknown {
  if (value === null) return null;
  return executeSelections(ctx, typeName, value, children);
}

function executeField(ctx: ExecCtx, typeName: string, value: Value, selection: Selection): unknown {
  const name = selection.name;
  const children = selection.children;

  // Root fields.
  if (typeName === 'Query') {
    if (name === 'viewer') return resolveObject(ctx, 'User', { __kind: 'viewer' }, children);
    if (name === 'repository') {
      const owner = argValue(selection, 'owner', ctx) as string | undefined;
      const repoName = argValue(selection, 'name', ctx) as string | undefined;
      if (owner !== VIEWER_LOGIN) return null;
      const repo = repoName !== undefined ? ctx.forge.getRepo(repoName) : undefined;
      return repo === undefined ? null : resolveObject(ctx, 'Repository', { __kind: 'repo', repo }, children);
    }
    if (name === 'node') {
      const id = argValue(selection, 'id', ctx) as string | undefined;
      if (id === undefined) return null;
      const parsed = parseGlobalId(id);
      if (parsed === null) return null;
      if (parsed.kind === 'repo') {
        const [owner, repoName] = parsed.key.split('/');
        if (owner !== VIEWER_LOGIN || repoName === undefined) return null;
        const repo = ctx.forge.getRepo(repoName);
        if (repo === undefined) return null;
        return resolveObject(ctx, 'Repository', { __kind: 'repo', repo }, children);
      }
      if (parsed.kind === 'issue' || parsed.kind === 'pull') {
        const match = /^([^/]+)\/([^:]+):(\d+)$/.exec(parsed.key);
        if (match === null) return null;
        const [, owner, repoName, numberText] = match;
        if (owner !== VIEWER_LOGIN) return null;
        const repo = ctx.forge.getRepo(repoName!);
        if (repo === undefined) return null;
        const number = Number(numberText);
        if (parsed.kind === 'issue') {
          const issue = repo.issues.find((entry) => entry.number === number);
          return issue === undefined ? null : resolveObject(ctx, 'Issue', { __kind: 'issue', repo, issue }, children);
        }
        const pull = repo.pulls.find((entry) => entry.number === number);
        return pull === undefined ? null : resolveObject(ctx, 'PullRequest', { __kind: 'pull', repo, pull }, children);
      }
      return null;
    }
    if (name === '__type') {
      const typeNameArg = argValue(selection, 'name', ctx) as string | undefined;
      if (typeNameArg === undefined || TYPE_KIND[typeNameArg] === undefined) return null;
      return resolveObject(ctx, '__Type', { __kind: 'gqlType', name: typeNameArg }, children);
    }
    return null;
  }

  // Mutations.
  if (typeName === 'Mutation') {
    const input = (argValue(selection, 'input', ctx) ?? {}) as { [key: string]: unknown };
    if (name === 'createIssue') {
      const repo = repoFromGlobalId(ctx, input.repositoryId as string | undefined);
      if (repo === null) return null;
      const title = (input.title as string | undefined) ?? '';
      const body = (input.body as string | undefined) ?? '';
      const created = ctx.forge.createIssue(repo, title, body, []);
      if ('error' in created) return null;
      return resolveObject(ctx, 'CreateIssuePayload', { __kind: 'payload', fields: { issue: { __kind: 'issue', repo, issue: created.issue } } }, children);
    }
    if (name === 'closeIssue') {
      const found = issueFromGlobalId(ctx, input.issueId as string | undefined);
      if (found === null) return null;
      found.issue.state = 'closed';
      dropIssueJsonCache(found.repo, found.issue.number);
      return resolveObject(ctx, 'CloseIssuePayload', { __kind: 'payload', fields: { issue: { __kind: 'issue', repo: found.repo, issue: found.issue } } }, children);
    }
    if (name === 'createPullRequest') {
      const repo = repoFromGlobalId(ctx, input.repositoryId as string | undefined);
      if (repo === null) return null;
      const head = (input.headRefName as string | undefined) ?? '';
      const base = (input.baseRefName as string | undefined) ?? DEFAULT_BRANCH;
      const title = (input.title as string | undefined) ?? '';
      const created = ctx.forge.createPull(repo, head, base, title, '');
      if ('error' in created) return null;
      return resolveObject(ctx, 'CreatePullRequestPayload', { __kind: 'payload', fields: { pullRequest: { __kind: 'pull', repo, pull: created.pull } } }, children);
    }
    if (name === 'mergePullRequest') {
      const found = pullFromGlobalId(ctx, input.pullRequestId as string | undefined);
      if (found === null) return null;
      const merged = ctx.forge.mergeSquash(found.repo, found.pull);
      if ('error' in merged) return null;
      return resolveObject(ctx, 'MergePullRequestPayload', { __kind: 'payload', fields: { pullRequest: { __kind: 'pull', repo: found.repo, pull: found.pull } } }, children);
    }
    if (name === 'addComment') {
      const found = issueFromGlobalId(ctx, input.subjectId as string | undefined);
      if (found === null) return null;
      const body = (input.body as string | undefined) ?? '';
      const comment = ctx.forge.addComment(found.repo, found.issue.number, body);
      if (comment === null) return null;
      return resolveObject(
        ctx,
        'AddCommentPayload',
        { __kind: 'payload', fields: { commentEdge: { __kind: 'payload', fields: { node: { __kind: 'comment', id: comment.id, body: comment.body } } } } },
        children,
      );
    }
    return null;
  }

  if (value === null) return null;

  // Payload wrappers (mutation results).
  if (value.__kind === 'payload') {
    const inner = value.fields[name] ?? null;
    if (inner === null) return null;
    return resolveObject(ctx, valueType(inner), inner, children);
  }

  // Scalars and objects by type.
  switch (value.__kind) {
    case 'viewer': {
      if (name === 'id') return globalId('user', VIEWER_LOGIN);
      if (name === 'login') return VIEWER_LOGIN;
      if (name === 'name') return 'ada iversen';
      if (name === 'email') return 'adaiversen@example.invalid';
      return null;
    }
    case 'repo': {
      const repo = value.repo;
      if (name === 'id') return globalId('repo', `${VIEWER_LOGIN}/${repo.name}`);
      if (name === 'databaseId') return repo.databaseId;
      if (name === 'name') return repo.name;
      if (name === 'nameWithOwner') return `${VIEWER_LOGIN}/${repo.name}`;
      if (name === 'owner') return resolveObject(ctx, 'User', { __kind: 'viewer' }, children);
      if (name === 'defaultBranchRef') return resolveObject(ctx, 'Ref', { __kind: 'ref', name: DEFAULT_BRANCH }, children);
      if (name === 'viewerPermission') return 'ADMIN';
      if (name === 'hasIssuesEnabled') return true;
      if (name === 'issue') {
        const number = argValue(selection, 'number', ctx) as number | undefined;
        const issue = number === undefined ? undefined : repo.issues.find((entry) => entry.number === number);
        return issue === undefined ? null : resolveObject(ctx, 'Issue', { __kind: 'issue', repo, issue }, children);
      }
      if (name === 'issues') {
        const page = paginateIssues(ctx, repo, selection);
        return resolveObject(ctx, 'IssueConnection', { __kind: 'issuesConn', repo, issues: page.items, hasNext: page.hasNext, endCursor: page.endCursor }, children);
      }
      if (name === 'pullRequests') {
        const page = paginatePulls(ctx, repo, selection);
        return resolveObject(ctx, 'PullRequestConnection', { __kind: 'pullsConn', repo, pulls: page.items, hasNext: page.hasNext, endCursor: page.endCursor }, children);
      }
      if (name === 'pullRequest') {
        const number = argValue(selection, 'number', ctx) as number | undefined;
        const pull = number === undefined ? undefined : repo.pulls.find((entry) => entry.number === number);
        return pull === undefined ? null : resolveObject(ctx, 'PullRequest', { __kind: 'pull', repo, pull }, children);
      }
      return null;
    }
    case 'issue': {
      const issue = value.issue;
      if (name === 'id') return globalId('issue', `${VIEWER_LOGIN}/${value.repo.name}:${issue.number}`);
      if (name === 'number') return issue.number;
      if (name === 'title') return issue.title;
      if (name === 'state') return issue.state.toUpperCase();
      if (name === 'body') return issue.body;
      if (name === 'repository') return resolveObject(ctx, 'Repository', { __kind: 'repo', repo: value.repo }, children);
      return null;
    }
    case 'pull': {
      const pull = value.pull;
      if (name === 'id') return globalId('pull', `${VIEWER_LOGIN}/${value.repo.name}:${pull.number}`);
      if (name === 'number') return pull.number;
      if (name === 'title') return pull.title;
      if (name === 'body') return pull.body;
      if (name === 'state') return pull.merged ? 'MERGED' : pull.state.toUpperCase();
      if (name === 'merged') return pull.merged;
      if (name === 'headRefName') return pull.head;
      if (name === 'baseRefName') return pull.base;
      if (name === 'mergeable') return ctx.forge.pullConflict(value.repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
      if (name === 'files') {
        const files = ctx.forge.pullFiles(value.repo, pull).map((file) => ({ path: file.path }));
        return resolveObject(ctx, 'PullRequestFileConnection', { __kind: 'filesConn', files }, children);
      }
      return null;
    }
    case 'issuesConn': {
      if (name === 'nodes') {
        return value.issues.map((issue) => resolveObject(ctx, 'Issue', { __kind: 'issue', repo: value.repo, issue }, children));
      }
      if (name === 'pageInfo') return resolveObject(ctx, 'PageInfo', { __kind: 'pageInfo', hasNext: value.hasNext, endCursor: value.endCursor }, children);
      if (name === 'totalCount') return value.repo.issues.length;
      return null;
    }
    case 'pullsConn': {
      if (name === 'nodes') {
        return value.pulls.map((pull) => resolveObject(ctx, 'PullRequest', { __kind: 'pull', repo: value.repo, pull }, children));
      }
      if (name === 'pageInfo') return resolveObject(ctx, 'PageInfo', { __kind: 'pageInfo', hasNext: value.hasNext, endCursor: value.endCursor }, children);
      if (name === 'totalCount') return value.repo.pulls.length;
      return null;
    }
    case 'filesConn': {
      if (name === 'nodes') return value.files.map((file) => ({ path: file.path }));
      if (name === 'totalCount') return value.files.length;
      return null;
    }
    case 'pageInfo': {
      if (name === 'hasNextPage') return value.hasNext;
      if (name === 'endCursor') return value.endCursor;
      if (name === 'hasPreviousPage') return false;
      if (name === 'startCursor') return null;
      return null;
    }
    case 'ref': {
      if (name === 'name') return value.name;
      return null;
    }
    case 'gqlType': {
      if (name === 'name') return value.name;
      if (name === 'kind') return TYPE_KIND[value.name] ?? 'OBJECT';
      if (name === 'fields') {
        const fields = FIELD_TYPES[value.name] ?? {};
        return Object.keys(fields).map((field) => ({ name: field, description: '' }));
      }
      if (name === 'enumValues') {
        const values = ENUM_VALUES[value.name] ?? [];
        return values.map((enumValue) => ({ name: enumValue, description: '' }));
      }
      return null;
    }
    case 'comment': {
      if (name === 'id') return globalId('comment', String(value.id));
      if (name === 'body') return value.body;
      return null;
    }
  }
  return null;
}

// ---------------------------------------------------------------------------------------------
// connection pagination + global-id lookups
// ---------------------------------------------------------------------------------------------

function paginateIssues(
  ctx: ExecCtx,
  repo: Repo,
  selection: Selection,
): { items: Issue[]; hasNext: boolean; endCursor: string | null } {
  const first = (argValue(selection, 'first', ctx) as number | undefined) ?? 10;
  const after = seqFromCursor((argValue(selection, 'after', ctx) as string | undefined) ?? null);
  const statesRaw = argValue(selection, 'states', ctx) as unknown[] | undefined;
  const orderBy = (argValue(selection, 'orderBy', ctx) ?? {}) as { field?: string; direction?: string };
  const direction = (orderBy.direction as string | undefined) ?? 'DESC';
  let list = [...repo.issues];
  if (statesRaw !== undefined) {
    const wanted = new Set(statesRaw.map((state) => String(state).toLowerCase()));
    list = list.filter((issue) => wanted.has(issue.state));
  }
  list.sort((a, b) => (direction === 'ASC' ? a.seq - b.seq : b.seq - a.seq));
  if (after !== null) list = direction === 'ASC' ? list.filter((issue) => issue.seq > after) : list.filter((issue) => issue.seq < after);
  const items = list.slice(0, first);
  const hasNext = list.length > items.length;
  const endCursor = items.length > 0 ? cursorFor(items[items.length - 1]!.seq) : null;
  return { items, hasNext, endCursor };
}

function paginatePulls(
  ctx: ExecCtx,
  repo: Repo,
  selection: Selection,
): { items: Pull[]; hasNext: boolean; endCursor: string | null } {
  const first = (argValue(selection, 'first', ctx) as number | undefined) ?? 10;
  const after = seqFromCursor((argValue(selection, 'after', ctx) as string | undefined) ?? null);
  let list = [...repo.pulls];
  list.sort((a, b) => b.seq - a.seq);
  if (after !== null) list = list.filter((pull) => pull.seq < after);
  const items = list.slice(0, first);
  const hasNext = list.length > items.length;
  const endCursor = items.length > 0 ? cursorFor(items[items.length - 1]!.seq) : null;
  return { items, hasNext, endCursor };
}

function repoFromGlobalId(ctx: ExecCtx, id: string | undefined): Repo | null {
  if (id === undefined) return null;
  const parsed = parseGlobalId(id);
  if (parsed === null || parsed.kind !== 'repo') return null;
  const [owner, repoName] = parsed.key.split('/');
  if (owner !== VIEWER_LOGIN || repoName === undefined) return null;
  return ctx.forge.getRepo(repoName) ?? null;
}

function issueFromGlobalId(ctx: ExecCtx, id: string | undefined): { repo: Repo; issue: Issue } | null {
  if (id === undefined) return null;
  const parsed = parseGlobalId(id);
  if (parsed === null || parsed.kind !== 'issue') return null;
  const match = /^([^/]+)\/([^:]+):(\d+)$/.exec(parsed.key);
  if (match === null) return null;
  const [, owner, repoName, numberText] = match;
  if (owner !== VIEWER_LOGIN) return null;
  const repo = ctx.forge.getRepo(repoName!);
  if (repo === undefined) return null;
  const issue = repo.issues.find((entry) => entry.number === Number(numberText));
  return issue === undefined ? null : { repo, issue };
}

function pullFromGlobalId(ctx: ExecCtx, id: string | undefined): { repo: Repo; pull: Pull } | null {
  if (id === undefined) return null;
  const parsed = parseGlobalId(id);
  if (parsed === null || parsed.kind !== 'pull') return null;
  const match = /^([^/]+)\/([^:]+):(\d+)$/.exec(parsed.key);
  if (match === null) return null;
  const [, owner, repoName, numberText] = match;
  if (owner !== VIEWER_LOGIN) return null;
  const repo = ctx.forge.getRepo(repoName!);
  if (repo === undefined) return null;
  const pull = repo.pulls.find((entry) => entry.number === Number(numberText));
  return pull === undefined ? null : { repo, pull };
}

// ---------------------------------------------------------------------------------------------
// entry point: one document, or a batch array of documents
// ---------------------------------------------------------------------------------------------

export interface GraphqlResult {
  status: number;
  body: unknown;
}

function executeDocument(forge: Forge, query: string, variables: { [key: string]: unknown }): { data?: unknown; errors?: { message: string }[] } {
  let parsed;
  try {
    parsed = new Parser(lex(query)).parseDocument();
  } catch (err) {
    return { errors: [{ message: err instanceof Error ? err.message : 'GraphQL syntax error' }] };
  }
  const operation = parsed.operations[0];
  if (operation === undefined) return { errors: [{ message: 'no operation found' }] };
  const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  const errors: string[] = [];
  validateSelections(rootType, operation.selections, parsed.fragments, errors);
  if (errors.length > 0) return { data: null, errors: errors.map((message) => ({ message })) };
  const ctx: ExecCtx = { forge, variables, fragments: parsed.fragments };
  const data = executeSelections(ctx, rootType, null, operation.selections);
  return { data };
}

/**
 * POST /api/graphql. Accepts one `{query, variables}` document or a JSON array of them
 * (batch), and answers each in order.
 */
export function handleGraphql(forge: Forge, body: unknown): GraphqlResult {
  if (Array.isArray(body)) {
    const results = body.map((entry) => {
      if (entry === null || typeof entry !== 'object') return { errors: [{ message: 'invalid request' }] };
      const doc = entry as { query?: unknown; variables?: unknown };
      if (typeof doc.query !== 'string') return { errors: [{ message: 'query is required' }] };
      const variables = doc.variables !== null && typeof doc.variables === 'object' ? (doc.variables as { [key: string]: unknown }) : {};
      return executeDocument(forge, doc.query, variables);
    });
    return { status: 200, body: results };
  }
  if (body === null || typeof body !== 'object') return { status: 400, body: { errors: [{ message: 'query is required' }] } };
  const doc = body as { query?: unknown; variables?: unknown };
  if (typeof doc.query !== 'string') return { status: 400, body: { errors: [{ message: 'query is required' }] } };
  const variables = doc.variables !== null && typeof doc.variables === 'object' ? (doc.variables as { [key: string]: unknown }) : {};
  return { status: 200, body: executeDocument(forge, doc.query, variables) };
}
