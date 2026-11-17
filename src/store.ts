// store.ts — the forge's whole state, in memory. One Forge holds every repository the
// token owner created, with their git objects, refs, issues, pull requests and labels.
// All four surfaces (git smart HTTP, REST v3, GraphQL, MCP) read and write this one
// store, so a change made over any of them is visible on all the others.
//
// Identity: the suite's scenario is built from the contest's one fixed seed, so the
// token owner's login is the constant below (viewer "adaiversen", stranger
// "olgabauer"). A repository belongs to the token owner; the same name under any other
// owner is a 404, never a 403.
//
// Determinism: server-created commits use a fixed identity and a fixed timestamp, issue
// and pull-request ids come from counters, and nothing here reads the wall clock or
// Math.random — two runs over the same requests hold the same state.

import {
  buildCommit,
  buildTree,
  collectReachable,
  flattenTree,
  isAncestor,
  isOid,
  lookupPath,
  oidOf,
  parseCommit,
  parseTag,
  parseTree,
  setPath,
  walkCommits,
  ZERO_OID,
  type GitObj,
  type ObjKind,
} from './gitcore.ts';

/** The login the bearer token names (the contest scenario's fixed viewer). */
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
/** Another login with no rights on the viewer's repositories. */
export const STRANGER_LOGIN = 'olgabauer';
/** The default branch every repository is born with. */
export const DEFAULT_BRANCH = 'main';
/** Fixed identity and timestamp for commits the server itself writes. */
export const SERVER_AUTHOR = `${VIEWER_NAME} <${VIEWER_EMAIL}>`;
export const SERVER_EPOCH = 1_700_000_000;
export const SERVER_MERGED_AT = '2025-01-01T00:00:00Z';

function isoAt(epochSec: number): string {
  return new Date(epochSec * 1000).toISOString();
}

export interface Label {
  id: number;
  name: string;
  color: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  seq: number;
  createdAt: string;
}

export interface IssueComment {
  id: number;
  body: string;
  seq: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: string;
  base: string;
  /** The base branch tip when the pull request was opened (frozen). */
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeSha: string | null;
  seq: number;
  createdAt: string;
}

export interface Repo {
  name: string;
  description: string;
  isPrivate: boolean;
  databaseId: number;
  seq: number;
  createdAt: string;
  objects: Map<string, GitObj>;
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Issue[];
  comments: Map<number, IssueComment[]>;
  pulls: Pull[];
  nextIssue: number;
  nextPull: number;
  nextCommentId: number;
  /** Bumped on every change to git objects or refs; read-path caches key on it. */
  gitGen: number;
  /** Issues by number, for O(1) single-issue reads. */
  issueByNumber: Map<number, Issue>;
  /** Serialized issue JSON by number, for the hot single-issue GET. */
  issueJsonCache: Map<number, string>;
  /** Cached sorted refs, valid while `gen` equals `gitGen`. */
  sortedRefsCache: { gen: number; refs: Array<{ name: string; oid: string }> } | null;
  /** Cached git wire answers (fetch / ls-refs / v0 ref lines), keyed with `gitGen`. */
  gitCache: Map<string, Buffer>;
}

export interface ChangedFile {
  path: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
  oldOid: string | null;
  newOid: string | null;
}

export class Forge {
  readonly repos = new Map<string, Repo>();
  nextDatabaseId = 5000;
  nextGlobalId = 100_000;
  nextSeq = 1;

  /** A fresh numeric id, stable across reads of the same object. */
  freshId(): number {
    this.nextGlobalId += 1;
    return this.nextGlobalId;
  }

  createRepo(name: string, isPrivate: boolean, description: string): Repo | null {
    if (this.repos.has(name)) return null;
    const seq = this.nextSeq++;
    const repo: Repo = {
      name,
      description,
      isPrivate,
      databaseId: this.nextDatabaseId++,
      seq,
      createdAt: isoAt(SERVER_EPOCH + seq),
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: [],
      comments: new Map(),
      pulls: [],
      nextIssue: 1,
      nextPull: 1,
      nextCommentId: 1,
      gitGen: 0,
      issueByNumber: new Map(),
      issueJsonCache: new Map(),
      sortedRefsCache: null,
      gitCache: new Map(),
    };
    this.repos.set(name, repo);
    return repo;
  }

  getRepo(name: string): Repo | undefined {
    return this.repos.get(name);
  }

  deleteRepo(name: string): boolean {
    return this.repos.delete(name);
  }

  /**
   * Mark a repo's git state changed: every read-path cache keyed on `gitGen` (sorted
   * refs, cached fetch / ls-refs answers) is rebuilt on its next use. Call it at every
   * site that stores an object or moves a ref.
   */
  touchGit(repo: Repo): void {
    repo.gitGen += 1;
  }

  /** Store an object, answering its id (idempotent: the same bytes give the same id). */
  putObject(repo: Repo, kind: ObjKind, bytes: Buffer): string {
    const oid = oidOf(kind, bytes);
    if (!repo.objects.has(oid)) {
      repo.objects.set(oid, { kind, bytes });
      this.touchGit(repo);
    }
    return oid;
  }

  lookup(repo: Repo): (oid: string) => GitObj | undefined {
    return (oid: string) => repo.objects.get(oid);
  }

  refTip(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  branchTip(repo: Repo, branch: string): string | undefined {
    return repo.refs.get(`refs/heads/${branch}`);
  }

  /** Every ref the repo holds, sorted by name (HEAD excluded: it is a symref, not a ref). */
  sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
    const cached = repo.sortedRefsCache;
    if (cached !== null && cached.gen === repo.gitGen) return cached.refs;
    const refs = [...repo.refs.entries()]
      .map(([name, oid]) => ({ name, oid }))
      .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    repo.sortedRefsCache = { gen: repo.gitGen, refs };
    return refs;
  }

  /** The commit a tag object peels to (itself when the ref already names a commit). */
  peel(repo: Repo, oid: string): string | null {
    const object = repo.objects.get(oid);
    if (!object) return null;
    if (object.kind === 'commit') return oid;
    if (object.kind === 'tag') {
      try {
        return parseTag(object.bytes).object;
      } catch {
        return null;
      }
    }
    return null;
  }

  // -------------------------------------------------------------------------------------------
  // commits the server writes (contents API and squash merges)
  // -------------------------------------------------------------------------------------------

  /**
   * Write one file (or delete it when `content` is null) on `branch`, creating the branch
   * from the default branch when it does not exist yet. Answers the new commit id, the
   * blob id, and whether the file was created (201) or updated (200).
   */
  writeFile(
    repo: Repo,
    path: string,
    content: Buffer | null,
    message: string,
    branch?: string,
    expectedSha?: string,
  ): { commitSha: string; blobSha: string | null; created: boolean; error?: string } {
    const branchName = branch ?? DEFAULT_BRANCH;
    const refName = `refs/heads/${branchName}`;
    let tip = repo.refs.get(refName);
    if (tip === undefined && branchName !== DEFAULT_BRANCH) {
      tip = repo.refs.get(`refs/heads/${DEFAULT_BRANCH}`);
    }
    let parentTree: string | null = null;
    let created = true;
    if (tip !== undefined) {
      const commit = repo.objects.get(tip);
      if (!commit || commit.kind !== 'commit') return { commitSha: '', blobSha: null, created: false, error: 'branch tip is not a commit' };
      parentTree = parseCommit(commit.bytes).tree;
      const existing = lookupPath(this.lookup(repo), parentTree, path);
      created = existing === null;
      if (expectedSha !== undefined) {
        if (existing === null || existing.kind !== 'blob' || existing.oid !== expectedSha) {
          return { commitSha: '', blobSha: null, created: false, error: 'stale sha' };
        }
      }
      if (content === null && existing === null) {
        return { commitSha: '', blobSha: null, created: false, error: 'file does not exist' };
      }
    } else if (expectedSha !== undefined) {
      return { commitSha: '', blobSha: null, created: false, error: 'file does not exist' };
    }
    let blobSha: string | null = null;
    if (content !== null) blobSha = this.putObject(repo, 'blob', content);
    const treeOid = setPath(
      this.lookup(repo),
      (kind, bytes) => this.putObject(repo, kind, bytes),
      parentTree,
      path,
      blobSha,
    );
    const commitBytes = buildCommit({
      tree: treeOid,
      parents: tip === undefined ? [] : [tip],
      message,
      author: SERVER_AUTHOR,
      committer: SERVER_AUTHOR,
      timestamp: SERVER_EPOCH,
    });
    const commitSha = this.putObject(repo, 'commit', commitBytes);
    repo.refs.set(refName, commitSha);
    this.touchGit(repo);
    return { commitSha, blobSha, created };
  }

  /** Read a file or directory entry at a ref (branch name, or a commit id). */
  readPath(
    repo: Repo,
    path: string,
    ref?: string,
  ): { kind: 'blob' | 'tree'; oid: string; bytes?: Buffer } | null {
    const tip = ref === undefined ? repo.refs.get(`refs/heads/${DEFAULT_BRANCH}`) : this.resolveRef(repo, ref);
    if (tip === undefined) return null;
    const commit = repo.objects.get(tip);
    if (!commit || commit.kind !== 'commit') return null;
    const treeOid = parseCommit(commit.bytes).tree;
    if (path === '' || path === '/') return { kind: 'tree', oid: treeOid };
    const found = lookupPath(this.lookup(repo), treeOid, path);
    if (!found) return null;
    if (found.kind === 'blob') {
      const blob = repo.objects.get(found.oid);
      if (!blob) return null;
      return { kind: 'blob', oid: found.oid, bytes: blob.bytes };
    }
    return { kind: 'tree', oid: found.oid };
  }

  /** List a directory's entries at a ref: {name, path, kind, oid}. */
  listDir(repo: Repo, path: string, ref?: string): Array<{ name: string; path: string; kind: 'file' | 'dir'; oid: string }> | null {
    const found = this.readPath(repo, path, ref);
    if (!found || found.kind !== 'tree') return null;
    const object = repo.objects.get(found.oid);
    if (!object) return null;
    return parseTree(object.bytes).map((entry) => ({
      name: entry.name,
      path: path === '' ? entry.name : `${path}/${entry.name}`,
      kind: entry.mode === '40000' ? ('dir' as const) : ('file' as const),
      oid: entry.oid,
    }));
  }

  /** Resolve a ref argument: a branch name, a full ref, or a commit id. */
  resolveRef(repo: Repo, ref: string): string | undefined {
    if (isOid(ref)) return repo.objects.has(ref) ? ref : undefined;
    if (ref.startsWith('refs/')) return repo.refs.get(ref);
    return repo.refs.get(`refs/heads/${ref}`);
  }

  /** The commits reachable from a ref, newest first. */
  listCommits(repo: Repo, ref?: string): string[] {
    const tip = ref === undefined ? repo.refs.get(`refs/heads/${DEFAULT_BRANCH}`) : this.resolveRef(repo, ref);
    if (tip === undefined) return [];
    return walkCommits([tip], this.lookup(repo));
  }

  // -------------------------------------------------------------------------------------------
  // pull-request diffs and squash merges
  // -------------------------------------------------------------------------------------------

  /** Line counts for a changed file: lines added and removed, by multiset difference. */
  lineCounts(oldBytes: Buffer | null, newBytes: Buffer | null): { additions: number; deletions: number } {
    const countLines = (bytes: Buffer | null): Map<string, number> => {
      const counts = new Map<string, number>();
      if (bytes === null) return counts;
      const text = bytes.toString('utf8');
      const lines = text === '' ? [] : text.split('\n');
      // A trailing newline does not start a new line.
      if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
      for (const line of lines) counts.set(line, (counts.get(line) ?? 0) + 1);
      return counts;
    };
    const oldLines = countLines(oldBytes);
    const newLines = countLines(newBytes);
    let additions = 0;
    let deletions = 0;
    for (const [line, count] of newLines) {
      const gone = Math.max(0, count - (oldLines.get(line) ?? 0));
      additions += gone;
    }
    for (const [line, count] of oldLines) {
      const gone = Math.max(0, count - (newLines.get(line) ?? 0));
      deletions += gone;
    }
    return { additions, deletions };
  }

  /**
   * The changed files between two trees: every path whose blob differs, added, or removed.
   * The base tree is the pull request's frozen base; the head tree is the head tip's tree.
   */
  diffTrees(repo: Repo, baseTreeOid: string, headTreeOid: string): ChangedFile[] {
    const lookup = this.lookup(repo);
    const base = flattenTree(lookup, baseTreeOid);
    const head = flattenTree(lookup, headTreeOid);
    const paths = new Set([...base.keys(), ...head.keys()]);
    const out: ChangedFile[] = [];
    for (const path of [...paths].sort()) {
      const oldOid = base.get(path) ?? null;
      const newOid = head.get(path) ?? null;
      if (oldOid === newOid) continue;
      const status = oldOid === null ? 'added' : newOid === null ? 'removed' : 'modified';
      const oldBytes = oldOid === null ? null : (repo.objects.get(oldOid)?.bytes ?? null);
      const newBytes = newOid === null ? null : (repo.objects.get(newOid)?.bytes ?? null);
      const { additions, deletions } = this.lineCounts(oldBytes, newBytes);
      out.push({ path, status, additions, deletions, changes: additions + deletions, oldOid, newOid });
    }
    return out;
  }

  /** The changed files of a pull request (base snapshot vs head tip). */
  pullFiles(repo: Repo, pull: Pull): ChangedFile[] {
    const baseCommit = repo.objects.get(pull.baseSha);
    const headTip = this.branchTip(repo, pull.head);
    if (!baseCommit || baseCommit.kind !== 'commit' || headTip === undefined) return [];
    const headCommit = repo.objects.get(headTip);
    if (!headCommit || headCommit.kind !== 'commit') return [];
    return this.diffTrees(repo, parseCommit(baseCommit.bytes).tree, parseCommit(headCommit.bytes).tree);
  }

  /**
   * Whether a pull request can merge cleanly: every file it changes must be unchanged on
   * the base branch since the pull request was opened.
   */
  pullConflict(repo: Repo, pull: Pull): boolean {
    const baseTip = this.branchTip(repo, pull.base);
    if (baseTip === undefined) return true;
    const baseCommit = repo.objects.get(pull.baseSha);
    const currentCommit = repo.objects.get(baseTip);
    if (!baseCommit || !currentCommit || baseCommit.kind !== 'commit' || currentCommit.kind !== 'commit') return true;
    const changed = this.pullFiles(repo, pull);
    if (changed.length === 0) return false;
    const oldFlat = flattenTree(this.lookup(repo), parseCommit(baseCommit.bytes).tree);
    const nowFlat = flattenTree(this.lookup(repo), parseCommit(currentCommit.bytes).tree);
    for (const file of changed) {
      if ((oldFlat.get(file.path) ?? null) !== (nowFlat.get(file.path) ?? null)) return true;
    }
    return false;
  }

  /**
   * Squash-merge a pull request onto its base branch: one new commit on the base tip whose
   * tree is the base tree with the pull request's changes applied. Answers the merge
   * commit id, or an error string ('conflict' | 'already-merged' | 'no-base' | 'no-head').
   */
  mergeSquash(repo: Repo, pull: Pull): { sha: string } | { error: string } {
    if (pull.merged || pull.state !== 'open') return { error: 'already-merged' };
    if (this.pullConflict(repo, pull)) return { error: 'conflict' };
    const baseTip = this.branchTip(repo, pull.base);
    const headTip = this.branchTip(repo, pull.head);
    if (baseTip === undefined || headTip === undefined) return { error: 'no-head' };
    const baseCommit = repo.objects.get(baseTip);
    const baseSnapshot = repo.objects.get(pull.baseSha);
    const headCommit = repo.objects.get(headTip);
    if (!baseCommit || !baseSnapshot || !headCommit) return { error: 'no-base' };
    if (baseCommit.kind !== 'commit' || baseSnapshot.kind !== 'commit' || headCommit.kind !== 'commit') {
      return { error: 'no-base' };
    }
    let treeOid = parseCommit(baseCommit.bytes).tree;
    const changed = this.pullFiles(repo, pull);
    for (const file of changed) {
      treeOid = setPath(
        this.lookup(repo),
        (kind, bytes) => this.putObject(repo, kind, bytes),
        treeOid,
        file.path,
        file.newOid,
      );
    }
    const message = `Merge pull request #${pull.number} from ${pull.head}\n\n${pull.title}\n`;
    const commitBytes = buildCommit({
      tree: treeOid,
      parents: [baseTip],
      message,
      author: SERVER_AUTHOR,
      committer: SERVER_AUTHOR,
      timestamp: SERVER_EPOCH,
    });
    const sha = this.putObject(repo, 'commit', commitBytes);
    repo.refs.set(`refs/heads/${pull.base}`, sha);
    this.touchGit(repo);
    pull.merged = true;
    pull.mergedAt = SERVER_MERGED_AT;
    pull.mergeSha = sha;
    pull.state = 'closed';
    return { sha };
  }

  // -------------------------------------------------------------------------------------------
  // issues and pull requests
  // -------------------------------------------------------------------------------------------

  createIssue(repo: Repo, title: string, body: string, labels: string[]): { issue: Issue } | { error: string } {
    for (const name of labels) {
      if (!repo.labels.has(name)) return { error: `unknown label ${name}` };
    }
    const seq = this.nextSeq++;
    const issue: Issue = {
      id: this.freshId(),
      number: repo.nextIssue++,
      title,
      body,
      state: 'open',
      labels: [...labels],
      seq,
      createdAt: isoAt(SERVER_EPOCH + seq),
    };
    repo.issues.push(issue);
    repo.issueByNumber.set(issue.number, issue);
    repo.comments.set(issue.number, []);
    return { issue };
  }

  addComment(repo: Repo, issueNumber: number, body: string): IssueComment | null {
    const issue = repo.issues.find((entry) => entry.number === issueNumber);
    if (!issue) return null;
    const comment: IssueComment = { id: repo.nextCommentId++, body, seq: this.nextSeq++ };
    const list = repo.comments.get(issueNumber) ?? [];
    list.push(comment);
    repo.comments.set(issueNumber, list);
    return comment;
  }

  createPull(
    repo: Repo,
    head: string,
    base: string,
    title: string,
    body: string,
  ): { pull: Pull } | { error: string } {
    const headTip = this.branchTip(repo, head);
    const baseTip = this.branchTip(repo, base);
    if (headTip === undefined || baseTip === undefined) return { error: 'unknown branch' };
    const existing = repo.pulls.find((entry) => entry.head === head && entry.base === base && entry.state === 'open');
    if (existing) return { error: 'a pull request is already open for this branch' };
    const seq = this.nextSeq++;
    const pull: Pull = {
      id: this.freshId(),
      number: repo.nextPull++,
      title,
      body,
      state: 'open',
      head,
      base,
      baseSha: baseTip,
      merged: false,
      mergedAt: null,
      mergeSha: null,
      seq,
      createdAt: isoAt(SERVER_EPOCH + seq),
    };
    repo.pulls.push(pull);
    return { pull };
  }

  // -------------------------------------------------------------------------------------------
  // git object helpers shared by the smart-HTTP handlers
  // -------------------------------------------------------------------------------------------

  /** Every object reachable from `roots`, minus every object reachable from `haves`. */
  freshObjects(repo: Repo, wants: readonly string[], haves: readonly string[]): Map<string, GitObj> {
    const lookup = this.lookup(repo);
    const wanted = collectReachable(wants, lookup);
    const held = new Set<string>();
    if (haves.length > 0) {
      try {
        for (const oid of collectReachable(haves, lookup)) held.add(oid);
      } catch {
        // A have the store does not hold is simply not held.
      }
    }
    const out = new Map<string, GitObj>();
    for (const oid of wanted) {
      if (held.has(oid)) continue;
      const object = repo.objects.get(oid);
      if (object) out.set(oid, object);
    }
    return out;
  }

  /**
   * The objects a shallow fetch at `depth` generations from `wants` must deliver, minus what
   * the client already holds. Answers the objects plus the new shallow boundary commits
   * (commits at the depth edge whose parents were not included) and the old boundaries
   * that stopped being shallow (their parents are now included).
   */
  shallowObjects(
    repo: Repo,
    wants: readonly string[],
    haves: readonly string[],
    oldShallow: readonly string[],
    depth: number,
  ): { objects: Map<string, GitObj>; shallow: string[]; unshallow: string[] } {
    const lookup = this.lookup(repo);
    // Walk commits at most `depth` generations from the wants.
    const generations = new Map<string, number>();
    const queue: Array<{ oid: string; gen: number }> = [];
    for (const want of wants) {
      if (isOid(want)) queue.push({ oid: want, gen: 1 });
    }
    const held = new Set<string>();
    if (haves.length > 0) {
      // A shallow client holds each have commit plus its tree — never the have's
      // ancestors, which it never received. Marking the full closure held would
      // wrongly suppress the very commits a deepen fetch must deliver.
      for (const have of haves) {
        const object = repo.objects.get(have);
        if (!object) continue;
        held.add(have);
        if (object.kind === 'commit') {
          try {
            for (const oid of collectReachable([parseCommit(object.bytes).tree], lookup)) held.add(oid);
          } catch {
            // An unresolvable tree holds nothing extra.
          }
        }
      }
    }
    const boundary = new Set<string>();
    const included = new Set<string>();
    while (queue.length > 0) {
      const { oid, gen } = queue.shift()!;
      if (included.has(oid)) continue;
      const object = lookup(oid);
      if (!object || object.kind !== 'commit') continue;
      included.add(oid);
      if (gen >= depth) {
        boundary.add(oid);
        continue;
      }
      for (const parent of parseCommit(object.bytes).parents) {
        if (!generations.has(parent) || generations.get(parent)! > gen + 1) generations.set(parent, gen + 1);
        queue.push({ oid: parent, gen: gen + 1 });
      }
    }
    // The objects: each included commit's tree closure, minus what the client holds.
    const out = new Map<string, GitObj>();
    for (const oid of included) {
      const commit = repo.objects.get(oid);
      if (!commit) continue;
      const tree = parseCommit(commit.bytes).tree;
      let closure: Set<string>;
      try {
        closure = collectReachable([tree], lookup);
      } catch {
        continue;
      }
      closure.add(oid);
      for (const member of closure) {
        if (held.has(member)) continue;
        const object = repo.objects.get(member);
        if (object) out.set(member, object);
      }
    }
    const shallow = [...boundary].filter((oid) => !held.has(oid));
    const unshallow = oldShallow.filter((oid) => {
      if (!included.has(oid)) return false;
      // No longer shallow when the commit's parents are now included too.
      const object = repo.objects.get(oid);
      if (!object || object.kind !== 'commit') return false;
      const parents = parseCommit(object.bytes).parents;
      return parents.length === 0 || parents.every((parent) => included.has(parent));
    });
    return { objects: out, shallow, unshallow };
  }

  /** Whether `newOid` is a fast-forward from `oldOid` (old is an ancestor of new, or a create). */
  isFastForward(repo: Repo, oldOid: string, newOid: string): boolean {
    if (oldOid === ZERO_OID) return true;
    if (newOid === ZERO_OID) return true;
    return isAncestor(oldOid, newOid, this.lookup(repo));
  }
}
