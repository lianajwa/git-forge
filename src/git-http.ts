// git-http.ts — git smart HTTP: info/refs advertisements (protocol v0 and v2),
// upload-pack (v2 ls-refs/fetch, v0 fetch) and receive-pack (push with report-status).
// The wire shapes here follow the git smart-HTTP protocol: pkt-line framing, sideband
// pack delivery, shallow/deepen negotiation, and per-ref push reports.

import {
  decodePkts,
  frameDelim,
  frameFlush,
  framePkt,
  framePkts,
  isOid,
  readPack,
  writePack,
  ZERO_OID,
  type GitObj,
} from './gitcore.ts';
import { DEFAULT_BRANCH, Forge, type Repo } from './store.ts';

const UPLOAD_AD = 'application/x-git-upload-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_AD = 'application/x-git-receive-pack-advertisement';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

export interface GitHttpResult {
  status: number;
  contentType: string;
  body: Buffer;
}

function ok(contentType: string, body: Buffer): GitHttpResult {
  return { status: 200, contentType, body };
}

function fail(status: number, message: string): GitHttpResult {
  return { status, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message }), 'utf8') };
}

/** The capabilities a v0 upload-pack advertisement carries (after NUL on the first line). */
const V0_UPLOAD_CAPS = 'side-band-64k ofs-delta thin-pack multi_ack shallow symref=HEAD:refs/heads/main';
/** The capabilities a v0 receive-pack advertisement carries. */
const V0_RECEIVE_CAPS = 'report-status side-band-64k delete-refs ofs-delta symref=HEAD:refs/heads/main';

/** The v2 capability advertisement (no refs: v2 lists them through ls-refs). Built once: it never varies. */
const V2_ADVERTISEMENT = framePkts(['version 2\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n', 'flush']);

/**
 * The read-path git cache: a repo's cached wire answers, keyed with its `gitGen` so any
 * object or ref change rebuilds them. Bounded (32 entries, 1 MiB each): when full it is
 * cleared and rebuilt, which keeps the hot clone keys live while memory stays small.
 * Every cached answer is a pure function of the repo's git state and the request's
 * arguments, so a hit answers byte-identically to a fresh build.
 */
const GIT_CACHE_MAX_ENTRIES = 32;
const GIT_CACHE_MAX_BYTES = 1_048_576;

function gitCacheGet(repo: Repo, key: string): Buffer | undefined {
  return repo.gitCache.get(key);
}

function gitCachePut(repo: Repo, key: string, value: Buffer): void {
  if (value.length > GIT_CACHE_MAX_BYTES) return;
  if (repo.gitCache.size >= GIT_CACHE_MAX_ENTRIES) repo.gitCache.clear();
  repo.gitCache.set(key, value);
}

/** One v0 ref line, the first carrying the capabilities after a NUL. */
function v0RefLines(repo: Repo, forge: Forge, caps: string): Buffer {
  const cached = gitCacheGet(repo, `v0:${repo.gitGen}:${caps}`);
  if (cached !== undefined) return cached;
  const refs = forge.sortedRefs(repo);
  const lines: string[] = [];
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
    const built = framePkts(lines.concat(['flush']));
    gitCachePut(repo, `v0:${repo.gitGen}:${caps}`, built);
    return built;
  }
  const defaultTip = forge.branchTip(repo, DEFAULT_BRANCH);
  const headLine = defaultTip !== undefined ? `${defaultTip} HEAD\0${caps}\n` : `${ZERO_OID} HEAD\0${caps}\n`;
  lines.push(headLine);
  for (const ref of refs) {
    lines.push(`${ref.oid} ${ref.name}\n`);
    // An annotated tag peels to its commit on a `^{}` line.
    const object = repo.objects.get(ref.oid);
    if (object && object.kind === 'tag') {
      const peeled = forge.peel(repo, ref.oid);
      if (peeled !== null) lines.push(`${peeled} ${ref.name}^{}\n`);
    }
  }
  const built = framePkts(lines.concat(['flush']));
  gitCachePut(repo, `v0:${repo.gitGen}:${caps}`, built);
  return built;
}

/**
 * GET /{owner}/{repo}.git/info/refs?service=git-upload-pack|git-receive-pack.
 * The Git-Protocol: version=2 header selects the v2 advertisement for upload-pack;
 * receive-pack is always v0.
 */
export function infoRefs(
  forge: Forge,
  owner: string,
  repoName: string,
  service: string | null,
  gitProtocol: string | null,
): GitHttpResult {
  if (owner !== 'adaiversen') return fail(404, 'Not Found');
  const repo = forge.getRepo(repoName);
  if (repo === undefined) return fail(404, 'Not Found');
  if (service === 'git-upload-pack') {
    if (gitProtocol === 'version=2') return ok(UPLOAD_AD, V2_ADVERTISEMENT);
    const head = framePkt('# service=git-upload-pack\n');
    return ok(UPLOAD_AD, Buffer.concat([head, frameFlush(), v0RefLines(repo, forge, V0_UPLOAD_CAPS)]));
  }
  if (service === 'git-receive-pack') {
    const head = framePkt('# service=git-receive-pack\n');
    return ok(RECEIVE_AD, Buffer.concat([head, frameFlush(), v0RefLines(repo, forge, V0_RECEIVE_CAPS)]));
  }
  return fail(400, 'unknown service');
}

// ---------------------------------------------------------------------------------------------
// v2 ls-refs
// ---------------------------------------------------------------------------------------------

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

function parseLsRefs(lines: string[]): LsRefsArgs {
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  let pastDelim = false;
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text === '') continue;
    if (text.startsWith('command=') || text.startsWith('agent=') || text.startsWith('object-format=')) continue;
    if (text === 'peel') args.peel = true;
    else if (text === 'symrefs') args.symrefs = true;
    else if (text === 'unborn') args.unborn = true;
    else if (text.startsWith('ref-prefix ')) args.prefixes.push(text.slice('ref-prefix '.length));
    void pastDelim;
  }
  return args;
}

/** The v2 ls-refs answer: HEAD, every ref, peeled tags, unborn HEAD for an empty repo. */
function lsRefsAnswer(forge: Forge, repo: Repo, args: LsRefsArgs): Buffer {
  const key = `ls:${repo.gitGen}:${args.peel ? 1 : 0}${args.symrefs ? 1 : 0}${args.unborn ? 1 : 0}:${[...args.prefixes].sort().join(',')}`;
  const cached = gitCacheGet(repo, key);
  if (cached !== undefined) return cached;
  const lines: string[] = [];
  const keep = (name: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const refs = forge.sortedRefs(repo);
  const defaultTip = forge.branchTip(repo, DEFAULT_BRANCH);
  if (refs.length === 0) {
    if (args.unborn && keep('HEAD')) lines.push('unborn HEAD symref-target:refs/heads/main\n');
  } else {
    if (keep('HEAD') && defaultTip !== undefined) {
      lines.push(`${defaultTip} HEAD symref-target:refs/heads/main\n`);
    }
    for (const ref of refs) {
      if (!keep(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      const object = repo.objects.get(ref.oid);
      if (args.peel && object && object.kind === 'tag') {
        const peeled = forge.peel(repo, ref.oid);
        if (peeled !== null) line += ` peeled:${peeled}`;
      }
      lines.push(`${line}\n`);
    }
  }
  lines.push('flush');
  const built = framePkts(lines);
  gitCachePut(repo, key, built);
  return built;
}

// ---------------------------------------------------------------------------------------------
// fetch (v2 and v0 share the pack-building core)
// ---------------------------------------------------------------------------------------------

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
}

/** Parse the arguments of a v2 fetch request (header lines ignored by prefix). */
function parseFetchV2(lines: string[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null };
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text.startsWith('want ')) args.wants.push(text.slice(5).trim());
    else if (text.startsWith('have ')) args.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) args.depth = Number(text.slice(7).trim());
  }
  return args;
}

/** Parse a v0 fetch request (want/have/shallow/deepen across two flushed sections). */
function parseFetchV0(lines: string[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null };
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text.startsWith('want ')) {
      const first = text.slice(5).split(' ')[0] ?? '';
      if (isOid(first)) args.wants.push(first);
    } else if (text.startsWith('have ')) args.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) args.depth = Number(text.slice(7).trim());
  }
  return args;
}

/** Split pack bytes into sideband band-1 packets (each well under the pkt-line max). */
function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  const chunk = 32768;
  for (let at = 0; at < pack.length; at += chunk) {
    const piece = pack.subarray(at, Math.min(at + chunk, pack.length));
    parts.push(framePkt(Buffer.concat([Buffer.from([1]), piece])));
  }
  if (parts.length === 0) parts.push(framePkt(Buffer.concat([Buffer.from([1]), pack])));
  return Buffer.concat(parts);
}

/** The pack a fetch delivers, with the shallow bookkeeping for depth requests. */
function buildFetchPack(
  forge: Forge,
  repo: Repo,
  args: FetchArgs,
): { pack: Buffer; shallow: string[]; unshallow: string[] } {
  if (args.depth !== null && args.depth > 0) {
    const result = forge.shallowObjects(repo, args.wants, args.haves, args.shallow, args.depth);
    const pack = writePack([...result.objects.values()]);
    return { pack, shallow: result.shallow, unshallow: result.unshallow };
  }
  // A plain fetch: the closure of the wants, minus the closure of the haves.
  const wanted = new Set<string>();
  for (const want of args.wants) {
    if (!isOid(want) || !repo.objects.has(want)) continue;
    wanted.add(want);
  }
  let objects: Map<string, GitObj>;
  try {
    objects = forge.freshObjects(repo, [...wanted], args.haves);
  } catch {
    objects = new Map();
  }
  return { pack: writePack([...objects.values()]), shallow: [], unshallow: [] };
}

/** The cache key for a fetch answer: the repo's git generation plus every argument that shapes it. */
function fetchCacheKey(repo: Repo, version: 2 | 0, args: FetchArgs): string {
  const sortJoin = (values: readonly string[]): string => [...values].sort().join(',');
  return `fetch${version}:${repo.gitGen}:${sortJoin(args.wants)}|${sortJoin(args.haves)}|${sortJoin(args.shallow)}|${args.depth ?? ''}`;
}

/** The v2 fetch answer: acknowledgments, shallow-info, packfile sections. */
function fetchV2Answer(forge: Forge, repo: Repo, args: FetchArgs): Buffer {
  const key = fetchCacheKey(repo, 2, args);
  const cached = gitCacheGet(repo, key);
  if (cached !== undefined) return cached;
  const { pack, shallow, unshallow } = buildFetchPack(forge, repo, args);
  const parts: Buffer[] = [];
  parts.push(framePkt('acknowledgments\n'));
  const held = args.haves.filter((have) => repo.objects.has(have));
  if (held.length === 0) {
    parts.push(framePkt('NAK\n'));
  } else {
    for (const have of held) parts.push(framePkt(`ACK ${have}\n`));
    parts.push(framePkt('ready\n'));
  }
  parts.push(frameDelim());
  if (shallow.length > 0 || unshallow.length > 0) {
    parts.push(framePkt('shallow-info\n'));
    for (const oid of shallow) parts.push(framePkt(`shallow ${oid}\n`));
    for (const oid of unshallow) parts.push(framePkt(`unshallow ${oid}\n`));
    parts.push(frameDelim());
  }
  parts.push(framePkt('packfile\n'));
  parts.push(sidebandPack(pack));
  parts.push(frameFlush());
  const built = Buffer.concat(parts);
  gitCachePut(repo, key, built);
  return built;
}

/** The v0 fetch answer: NAK/ACK/shallow lines, then the sideband pack. */
function fetchV0Answer(forge: Forge, repo: Repo, args: FetchArgs): Buffer {
  const key = fetchCacheKey(repo, 0, args);
  const cached = gitCacheGet(repo, key);
  if (cached !== undefined) return cached;
  const { pack, shallow, unshallow } = buildFetchPack(forge, repo, args);
  const parts: Buffer[] = [];
  const held = args.haves.filter((have) => repo.objects.has(have));
  if (held.length === 0) parts.push(framePkt('NAK\n'));
  else for (const have of held) parts.push(framePkt(`ACK ${have}\n`));
  for (const oid of shallow) parts.push(framePkt(`shallow ${oid}\n`));
  for (const oid of unshallow) parts.push(framePkt(`unshallow ${oid}\n`));
  parts.push(sidebandPack(pack));
  parts.push(frameFlush());
  const built = Buffer.concat(parts);
  gitCachePut(repo, key, built);
  return built;
}

// ---------------------------------------------------------------------------------------------
// upload-pack POST (ls-refs / fetch) and receive-pack POST (push)
// ---------------------------------------------------------------------------------------------

/**
 * POST /{owner}/{repo}.git/git-upload-pack. The first line selects the shape: a v2
 * `command=ls-refs` or `command=fetch` request, or a v0 want/have request.
 */
export function uploadPack(forge: Forge, owner: string, repoName: string, body: Buffer): GitHttpResult {
  if (owner !== 'adaiversen') return fail(404, 'Not Found');
  const repo = forge.getRepo(repoName);
  if (repo === undefined) return fail(404, 'Not Found');
  let packets;
  try {
    packets = decodePkts(body).packets;
  } catch {
    return fail(400, 'malformed pkt-line stream');
  }
  const lines: string[] = [];
  for (const packet of packets) if (packet.kind === 'data') lines.push(packet.payload.toString('utf8'));
  const first = (lines[0] ?? '').replace(/\n$/, '');
  if (first === 'command=ls-refs') {
    return ok(UPLOAD_RESULT, lsRefsAnswer(forge, repo, parseLsRefs(lines)));
  }
  if (first === 'command=fetch') {
    return ok(UPLOAD_RESULT, fetchV2Answer(forge, repo, parseFetchV2(lines)));
  }
  // v0: want/have/shallow/deepen lines in two flushed sections.
  return ok(UPLOAD_RESULT, fetchV0Answer(forge, repo, parseFetchV0(lines)));
}

/**
 * POST /{owner}/{repo}.git/git-receive-pack. A pkt-framed command list, a flush, then the
 * raw pack bytes. Each command is validated (old tip matches, new tip fast-forwards)
 * and the report answers one ok/ng line per ref, inside sideband band 1.
 */
export function receivePack(forge: Forge, owner: string, repoName: string, body: Buffer): GitHttpResult {
  if (owner !== 'adaiversen') return fail(404, 'Not Found');
  const repo = forge.getRepo(repoName);
  if (repo === undefined) return fail(404, 'Not Found');
  let packets;
  let tail: Buffer;
  try {
    const decoded = decodePkts(body, true);
    packets = decoded.packets;
    tail = decoded.tail;
  } catch {
    return fail(400, 'malformed pkt-line stream');
  }
  interface Command {
    oldOid: string;
    newOid: string;
    ref: string;
  }
  const commands: Command[] = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let text = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = text.indexOf('\0');
    if (nul >= 0) text = text.slice(0, nul);
    const parts = text.split(' ');
    if (parts.length < 3 || !isOid(parts[0]!) || !isOid(parts[1]!)) continue;
    commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
  }
  if (commands.length === 0) return fail(400, 'no push commands');

  // Unpack first: the pack's objects land in the store even when a ref is refused.
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (tail.length > 0) {
    try {
      const read = readPack(tail, (oid) => repo.objects.get(oid));
      let stored = false;
      for (const [oid, object] of read.objects) {
        if (!repo.objects.has(oid)) {
          repo.objects.set(oid, object);
          stored = true;
        }
      }
      if (stored) forge.touchGit(repo);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpack failed';
    }
  }

  const reportLines: string[] = [`unpack ${unpackOk ? 'ok' : unpackDetail}\n`];
  for (const command of commands) {
    let verdict: { ok: true } | { ok: false; reason: string } | null = null;
    if (!command.ref.startsWith('refs/')) {
      verdict = { ok: false, reason: 'invalid ref' };
    } else if (!unpackOk) {
      verdict = { ok: false, reason: 'unpack failed' };
    } else {
      const current = repo.refs.get(command.ref);
      if (command.newOid === ZERO_OID) {
        // A delete: the old tip must match.
        if (current === undefined) verdict = { ok: false, reason: 'non-fast-forward' };
        else if (current !== command.oldOid) verdict = { ok: false, reason: 'non-fast-forward' };
        else verdict = { ok: true };
      } else {
        const target = repo.objects.get(command.newOid);
        if (!target || (target.kind !== 'commit' && target.kind !== 'tag')) {
          verdict = { ok: false, reason: 'object not found' };
        } else if (current === undefined) {
          verdict = command.oldOid === ZERO_OID ? { ok: true } : { ok: false, reason: 'non-fast-forward' };
        } else if (command.oldOid === ZERO_OID) {
          verdict = { ok: false, reason: 'already exists' };
        } else if (current !== command.oldOid) {
          verdict = { ok: false, reason: 'non-fast-forward' };
        } else if (!forge.isFastForward(repo, command.oldOid, command.newOid)) {
          verdict = { ok: false, reason: 'non-fast-forward' };
        } else {
          verdict = { ok: true };
        }
      }
    }
    if (verdict !== null && verdict.ok) {
      if (command.newOid === ZERO_OID) repo.refs.delete(command.ref);
      else repo.refs.set(command.ref, command.newOid);
      forge.touchGit(repo);
      reportLines.push(`ok ${command.ref}\n`);
    } else {
      const reason = verdict !== null && !verdict.ok ? verdict.reason : 'failed';
      reportLines.push(`ng ${command.ref} ${reason}\n`);
    }
  }

  const reportBody = framePkts(reportLines);
  const sideband = framePkt(Buffer.concat([Buffer.from([1]), reportBody]));
  return ok(RECEIVE_RESULT, Buffer.concat([sideband, frameFlush()]));
}
