// gitcore.ts — the git object layer: SHA-1 object ids, pkt-line framing, packfiles,
// and commit/tree/tag parsing and building. Everything here is plain Node stdlib
// (node:crypto for SHA-1, node:zlib for pack deflate/inflate). No wall clock, no
// randomness: the same bytes in always give the same bytes out.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjKind = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObj {
  kind: ObjKind;
  bytes: Buffer;
}

export const ZERO_OID = '0'.repeat(40);
const HEX40 = /^[0-9a-f]{40}$/;

export function isOid(value: string): boolean {
  return HEX40.test(value);
}

/** git's object id: SHA-1 over `<kind> <size>\0` + the object's bytes. */
export function oidOf(kind: ObjKind, bytes: Uint8Array): string {
  const body = Buffer.isBuffer(bytes) ? bytes : Buffer.from(bytes);
  const hash = createHash('sha1');
  hash.update(Buffer.from(`${kind} ${body.length}\0`, 'utf8'));
  hash.update(body);
  return hash.digest('hex');
}

// ---------------------------------------------------------------------------------------------
// pkt-line framing (git's wire framing: 4 hex length bytes, 0000 flush, 0001 delim, 0002 end)
// ---------------------------------------------------------------------------------------------

export const PKT_MAX = 65520;

export type Pkt =
  | { kind: 'data'; payload: Buffer }
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'end' };

/** Frame one data payload as a pkt-line. Payloads longer than the max are an error. */
export function framePkt(payload: string | Uint8Array): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : Buffer.from(payload);
  if (data.length > PKT_MAX - 4) throw new Error(`pkt-line payload of ${data.length} bytes exceeds the limit`);
  const head = Buffer.alloc(4);
  head.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, data]);
}

export function frameFlush(): Buffer {
  return Buffer.from('0000', 'ascii');
}

export function frameDelim(): Buffer {
  return Buffer.from('0001', 'ascii');
}

/** Frame a list of lines/control packets in order. */
export function framePkts(items: ReadonlyArray<string | Uint8Array | 'flush' | 'delim' | 'end'>): Buffer {
  const parts: Buffer[] = [];
  for (const item of items) {
    if (item === 'flush') parts.push(frameFlush());
    else if (item === 'delim') parts.push(frameDelim());
    else if (item === 'end') parts.push(Buffer.from('0002', 'ascii'));
    else parts.push(framePkt(item));
  }
  return Buffer.concat(parts);
}

/**
 * Decode a whole buffer into packets. `allowRawTail` lets a receive-pack body keep its raw
 * pack bytes after the command list's flush packet (the pack is not pkt-framed).
 */
export function decodePkts(buf: Buffer, allowRawTail = false): { packets: Pkt[]; tail: Buffer } {
  const packets: Pkt[] = [];
  let at = 0;
  while (at < buf.length) {
    if (at + 4 > buf.length) throw new Error('truncated pkt-line header');
    const head = buf.subarray(at, at + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(head)) {
      if (allowRawTail) break;
      throw new Error(`bad pkt-line length ${JSON.stringify(head)}`);
    }
    const len = parseInt(head, 16);
    if (len === 0) {
      packets.push({ kind: 'flush' });
      at += 4;
      continue;
    }
    if (len === 1) {
      packets.push({ kind: 'delim' });
      at += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'end' });
      at += 4;
      continue;
    }
    if (len === 3 || len > PKT_MAX) throw new Error(`bad pkt-line length ${len}`);
    if (at + len > buf.length) throw new Error('truncated pkt-line payload');
    packets.push({ kind: 'data', payload: buf.subarray(at + 4, at + len) });
    at += len;
  }
  return { packets, tail: buf.subarray(at) };
}

/** The data payloads of a packet list, as text lines. */
export function pktLines(packets: readonly Pkt[]): string[] {
  const out: string[] = [];
  for (const p of packets) if (p.kind === 'data') out.push(p.payload.toString('utf8'));
  return out;
}

// ---------------------------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------------------------

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

/** Parse a tree object's bytes into entries, in stored order. */
export function parseTree(bytes: Uint8Array): TreeEntry[] {
  const buf = Buffer.isBuffer(bytes) ? bytes : Buffer.from(bytes);
  const entries: TreeEntry[] = [];
  let at = 0;
  while (at < buf.length) {
    const space = buf.indexOf(0x20, at);
    if (space < 0) throw new Error('tree entry has no mode separator');
    const nul = buf.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > buf.length) throw new Error('tree entry is truncated');
    entries.push({
      mode: buf.subarray(at, space).toString('ascii'),
      name: buf.subarray(space + 1, nul).toString('utf8'),
      oid: buf.subarray(nul + 1, nul + 21).toString('hex'),
    });
    at = nul + 21;
  }
  return entries;
}

/** git's tree sort: byte order of the name, a directory compared as if it ended in '/'. */
function treeSortKey(entry: TreeEntry): Buffer {
  return Buffer.from(entry.mode === '40000' ? `${entry.name}/` : entry.name, 'utf8');
}

/** Build a tree object's bytes from entries, sorted the way git sorts them. */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => Buffer.compare(treeSortKey(a), treeSortKey(b)));
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    if (!isOid(entry.oid)) throw new Error(`tree entry ${entry.name} has a bad object id`);
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------------------------

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

/** Parse a commit object's bytes. */
export function parseCommit(bytes: Uint8Array): CommitView {
  const buf = Buffer.isBuffer(bytes) ? bytes : Buffer.from(bytes);
  const blank = buf.indexOf('\n\n');
  const headEnd = blank < 0 ? buf.length : blank;
  const message = blank < 0 ? '' : buf.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of buf.subarray(0, headEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    const key = space < 0 ? line : line.slice(0, space);
    const value = space < 0 ? '' : line.slice(space + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  if (!isOid(tree)) throw new Error('commit has no valid tree header');
  return { tree, parents, author, committer, message };
}

export interface TagView {
  object: string;
  type: ObjKind;
  tag: string;
  tagger: string;
  message: string;
}

/** Parse an annotated tag object's bytes. */
export function parseTag(bytes: Uint8Array): TagView {
  const buf = Buffer.isBuffer(bytes) ? bytes : Buffer.from(bytes);
  const blank = buf.indexOf('\n\n');
  const headEnd = blank < 0 ? buf.length : blank;
  const headers = new Map<string, string>();
  for (const line of buf.subarray(0, headEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    if (space < 0) throw new Error(`tag header ${JSON.stringify(line)} has no value`);
    headers.set(line.slice(0, space), line.slice(space + 1));
  }
  const object = headers.get('object') ?? '';
  const type = headers.get('type') ?? '';
  if (!isOid(object)) throw new Error('tag has no valid object header');
  if (type !== 'commit' && type !== 'tree' && type !== 'blob' && type !== 'tag') {
    throw new Error(`tag has bad type ${JSON.stringify(type)}`);
  }
  return {
    object,
    type: type as ObjKind,
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : buf.subarray(blank + 2).toString('utf8'),
  };
}

export interface CommitFields {
  tree: string;
  parents?: readonly string[];
  message: string;
  author: string;
  committer: string;
  /** Unix seconds; callers pass a fixed constant, never the wall clock. */
  timestamp: number;
  timezone?: string;
}

/** Build a commit object's bytes. The message is used verbatim (callers end it with \n). */
export function buildCommit(fields: CommitFields): Buffer {
  if (!isOid(fields.tree)) throw new Error('buildCommit needs a valid tree id');
  const tz = fields.timezone ?? '+0000';
  const lines = [`tree ${fields.tree}`];
  for (const parent of fields.parents ?? []) {
    if (!isOid(parent)) throw new Error('buildCommit needs valid parent ids');
    lines.push(`parent ${parent}`);
  }
  lines.push(`author ${fields.author} ${fields.timestamp} ${tz}`);
  lines.push(`committer ${fields.committer} ${fields.timestamp} ${tz}`);
  lines.push('');
  return Buffer.from(`${lines.join('\n')}\n${fields.message}`, 'utf8');
}

/** Build an annotated tag object's bytes. */
export function buildTag(fields: {
  object: string;
  type: ObjKind;
  tag: string;
  message: string;
  tagger: string;
  timestamp: number;
  timezone?: string;
}): Buffer {
  if (!isOid(fields.object)) throw new Error('buildTag needs a valid object id');
  const tz = fields.timezone ?? '+0000';
  return Buffer.from(
    `object ${fields.object}\ntype ${fields.type}\ntag ${fields.tag}\ntagger ${fields.tagger} ${fields.timestamp} ${tz}\n\n${fields.message}`,
    'utf8',
  );
}

// ---------------------------------------------------------------------------------------------
// packfiles (version 2, no deltas on write; OFS/REF deltas resolved on read)
// ---------------------------------------------------------------------------------------------

const TYPE_CODE: Readonly<Record<ObjKind, number>> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Readonly<Record<number, ObjKind>> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** Encode one object's pack header: the type/size varint git's pack format uses. */
function packObjectHeader(kind: ObjKind, size: number): Buffer {
  const code = TYPE_CODE[kind];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a packfile from plain objects (no deltas). Order is the caller's. */
export function writePack(objects: readonly GitObj[]): Buffer {
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  const parts: Buffer[] = [header];
  for (const object of objects) {
    parts.push(packObjectHeader(object.kind, object.bytes.length));
    parts.push(deflateSync(object.bytes));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

/** SHA-1 of every pack byte before the 20-byte trailer. */
export function packChecksum(pack: Uint8Array): string {
  const buf = Buffer.isBuffer(pack) ? pack : Buffer.from(pack);
  if (buf.length < 20) throw new Error('pack is shorter than its trailer');
  return createHash('sha1').update(buf.subarray(0, buf.length - 20)).digest('hex');
}

/** Whether a pack's trailer matches its bytes. */
export function verifyPack(pack: Uint8Array): boolean {
  const buf = Buffer.isBuffer(pack) ? pack : Buffer.from(pack);
  if (buf.length < 32) return false;
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') return false;
  const trailer = buf.subarray(buf.length - 20).toString('hex');
  return trailer === packChecksum(buf);
}

function readPackVarint(buf: Buffer, start: number): { size: number; next: number; first: number } {
  if (start >= buf.length) throw new Error('pack entry has no size header');
  let byte = buf[start]!;
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let at = start + 1;
  while (byte & 0x80) {
    if (at >= buf.length) throw new Error('pack entry size varint is truncated');
    byte = buf[at++]!;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
  }
  return { size, next: at, first };
}

function readOfsBase(buf: Buffer, start: number): { baseOffset: number; next: number } {
  if (start >= buf.length) throw new Error('ofs-delta has no base offset');
  let byte = buf[start]!;
  let at = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    if (at >= buf.length) throw new Error('ofs-delta base offset is truncated');
    byte = buf[at++]!;
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { baseOffset: value, next: at };
}

/** Inflate one zlib stream at `offset`; returns its bytes and the offset just past the stream. */
function inflateAt(buf: Buffer, offset: number): { data: Buffer; next: number } {
  let result: { buffer: Buffer; engine: { bytesWritten: number } };
  try {
    result = inflateSync(buf.subarray(offset), { info: true }) as unknown as {
      buffer: Buffer;
      engine: { bytesWritten: number };
    };
  } catch (err) {
    throw new Error(`zlib stream at offset ${offset} did not inflate: ${(err as Error).message}`);
  }
  if (!result.engine.bytesWritten) throw new Error(`zlib stream at offset ${offset} consumed no bytes`);
  return { data: result.buffer, next: offset + result.engine.bytesWritten };
}

/** Apply a git delta (copy/insert instruction stream) to a base object's bytes. */
export function applyDelta(base: Uint8Array, delta: Uint8Array): Buffer {
  const baseBuf = Buffer.isBuffer(base) ? base : Buffer.from(base);
  const deltaBuf = Buffer.isBuffer(delta) ? delta : Buffer.from(delta);
  let at = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      if (at >= deltaBuf.length) throw new Error('delta size varint is truncated');
      const byte = deltaBuf[at++]!;
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== baseBuf.length) throw new Error(`delta expects a ${sourceSize}-byte base, got ${baseBuf.length}`);
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (at < deltaBuf.length) {
    const op = deltaBuf[at++]!;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) copyOffset += deltaBuf[at++]! * 2 ** shift;
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) copySize += deltaBuf[at++]! * 2 ** shift;
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      if (copyOffset + copySize > baseBuf.length) throw new Error('delta copy is out of the base');
      if (written + copySize > targetSize) throw new Error('delta writes past its target size');
      baseBuf.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      if (at + literal > deltaBuf.length) throw new Error('delta insert is truncated');
      if (written + literal > targetSize) throw new Error('delta writes past its target size');
      deltaBuf.copy(out, written, at, at + literal);
      at += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0 is invalid');
    }
  }
  if (written !== targetSize) throw new Error(`delta produced ${written} of ${targetSize} bytes`);
  return out;
}

export interface PackRead {
  version: number;
  declaredCount: number;
  objects: Map<string, GitObj>;
  deltaCount: number;
}

/**
 * Read a packfile (version 2 or 3) and resolve every object, including OFS_DELTA and REF_DELTA.
 * `resolveBase` supplies a base object a thin pack leaves out. Every object's SHA-1 is
 * recomputed; a mismatch is an error.
 */
export function readPack(pack: Uint8Array, resolveBase?: (oid: string) => GitObj | undefined): PackRead {
  const buf = Buffer.isBuffer(pack) ? pack : Buffer.from(pack);
  if (buf.length < 32) throw new Error(`pack is ${buf.length} bytes, too short`);
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack does not start with PACK');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const declaredCount = buf.readUInt32BE(8);
  const trailerStart = buf.length - 20;
  if (buf.subarray(trailerStart).toString('hex') !== packChecksum(buf)) {
    throw new Error('pack trailer does not match its bytes');
  }

  interface Raw {
    offset: number;
    typeCode: number;
    size: number;
    data: Buffer;
    baseOffset?: number;
    baseOid?: string;
    resolved?: GitObj;
    oid?: string;
  }
  const raw: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let at = 12;
  for (let index = 0; index < declaredCount; index += 1) {
    const entryOffset = at;
    const header = readPackVarint(buf, at);
    at = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: Raw = { offset: entryOffset, typeCode, size: header.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsBase(buf, at);
      at = base.next;
      if (base.baseOffset <= 0 || base.baseOffset > entryOffset) {
        throw new Error(`ofs-delta at ${entryOffset} names an invalid base offset ${base.baseOffset}`);
      }
      entry.baseOffset = entryOffset - base.baseOffset;
    } else if (typeCode === 7) {
      if (at + 20 > trailerStart) throw new Error('ref-delta base id is truncated');
      entry.baseOid = buf.subarray(at, at + 20).toString('hex');
      at += 20;
    } else if (!CODE_TYPE[typeCode]) {
      throw new Error(`pack entry ${index} has unknown type code ${typeCode}`);
    }
    const inflated = inflateAt(buf.subarray(0, trailerStart), at);
    if (inflated.data.length !== header.size) {
      throw new Error(`pack entry at ${entryOffset} declares ${header.size} bytes but inflated to ${inflated.data.length}`);
    }
    entry.data = inflated.data;
    at = inflated.next;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  if (at !== trailerStart) throw new Error(`pack has ${trailerStart - at} stray bytes before its trailer`);

  const objects = new Map<string, GitObj>();
  const resolvedByOid = new Map<string, Raw>();
  const resolving = new Set<number>();
  let deltaCount = 0;
  const resolve = (entry: Raw): GitObj & { oid: string } => {
    if (entry.resolved && entry.oid) return { ...entry.resolved, oid: entry.oid };
    if (resolving.has(entry.offset)) throw new Error(`delta cycle at pack offset ${entry.offset}`);
    resolving.add(entry.offset);
    let kind: ObjKind;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset!);
      if (!baseEntry) throw new Error(`ofs-delta base at offset ${entry.baseOffset} is not in the pack`);
      const base = resolve(baseEntry);
      kind = base.kind;
      data = applyDelta(base.bytes, entry.data);
    } else if (entry.typeCode === 7) {
      const wanted = entry.baseOid!;
      const baseEntry = resolvedByOid.get(wanted);
      if (baseEntry) {
        const base = resolve(baseEntry);
        kind = base.kind;
        data = applyDelta(base.bytes, entry.data);
      } else {
        const external = resolveBase?.(wanted);
        if (!external) throw new Error(`ref-delta base ${wanted} is neither in the pack nor supplied`);
        kind = external.kind;
        data = applyDelta(external.bytes, entry.data);
      }
    } else {
      kind = CODE_TYPE[entry.typeCode]!;
      data = entry.data;
    }
    const oid = oidOf(kind, data);
    entry.resolved = { kind, bytes: data };
    entry.oid = oid;
    resolvedByOid.set(oid, entry);
    resolving.delete(entry.offset);
    return { kind, bytes: data, oid };
  };
  for (const entry of raw) {
    const resolved = resolve(entry);
    if (entry.typeCode === 6 || entry.typeCode === 7) deltaCount += 1;
    if (!objects.has(resolved.oid)) objects.set(resolved.oid, { kind: resolved.kind, bytes: resolved.bytes });
  }
  return { version, declaredCount, objects, deltaCount };
}

// ---------------------------------------------------------------------------------------------
// reachability and tree paths
// ---------------------------------------------------------------------------------------------

export type ObjectLookup = (oid: string) => GitObj | undefined;

/**
 * Every object reachable from `roots`: commit → tree + parents, tree → entries (gitlinks
 * skipped), tag → its target, blob → nothing. Throws on a missing object.
 */
export function collectReachable(roots: Iterable<string>, lookup: ObjectLookup): Set<string> {
  const seen = new Set<string>();
  const queue: string[] = [];
  for (const root of roots) if (isOid(root)) queue.push(root);
  while (queue.length > 0) {
    const oid = queue.pop()!;
    if (seen.has(oid)) continue;
    const object = lookup(oid);
    if (!object) throw new Error(`object ${oid} is missing from the store`);
    seen.add(oid);
    if (object.kind === 'commit') {
      const commit = parseCommit(object.bytes);
      queue.push(commit.tree);
      for (const parent of commit.parents) queue.push(parent);
    } else if (object.kind === 'tag') {
      queue.push(parseTag(object.bytes).object);
    } else if (object.kind === 'tree') {
      for (const entry of parseTree(object.bytes)) {
        if (entry.mode === '160000') continue;
        queue.push(entry.oid);
      }
    }
  }
  return seen;
}

/** The commit-generation walk from `roots`: commit ids in newest-first order (parents after). */
export function walkCommits(roots: Iterable<string>, lookup: ObjectLookup): string[] {
  const seen = new Set<string>();
  const order: string[] = [];
  const queue: string[] = [];
  for (const root of roots) if (isOid(root)) queue.push(root);
  while (queue.length > 0) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    const object = lookup(oid);
    if (!object || object.kind !== 'commit') continue;
    seen.add(oid);
    order.push(oid);
    for (const parent of parseCommit(object.bytes).parents) queue.push(parent);
  }
  return order;
}

/** True when `ancestor` is reachable from `tip` (or equal to it). */
export function isAncestor(ancestor: string, tip: string, lookup: ObjectLookup): boolean {
  if (ancestor === tip) return true;
  if (ancestor === ZERO_OID) return true;
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length > 0) {
    const oid = queue.pop()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = lookup(oid);
    if (!object || object.kind !== 'commit') continue;
    for (const parent of parseCommit(object.bytes).parents) {
      if (parent === ancestor) return true;
      queue.push(parent);
    }
  }
  return false;
}

export interface PathLookup {
  kind: 'blob' | 'tree';
  oid: string;
}

/** Resolve a slash path inside a tree object; null when absent. */
export function lookupPath(lookup: ObjectLookup, treeOid: string, path: string): PathLookup | null {
  const segments = path.split('/').filter((part) => part !== '');
  if (segments.length === 0) return { kind: 'tree', oid: treeOid };
  let current = treeOid;
  for (let index = 0; index < segments.length; index += 1) {
    const object = lookup(current);
    if (!object || object.kind !== 'tree') return null;
    const entry = parseTree(object.bytes).find((candidate) => candidate.name === segments[index]);
    if (!entry) return null;
    if (index === segments.length - 1) {
      return { kind: entry.mode === '40000' ? 'tree' : 'blob', oid: entry.oid };
    }
    if (entry.mode !== '40000') return null;
    current = entry.oid;
  }
  return null;
}

/**
 * Return a new tree oid with `path` set to `blobOid` (or removed when null), building the
 * intermediate trees. `put` stores each new object and answers its oid. The input tree may be
 * null for an empty tree.
 */
export function setPath(
  lookup: ObjectLookup,
  put: (kind: ObjKind, bytes: Buffer) => string,
  treeOid: string | null,
  path: string,
  blobOid: string | null,
): string {
  const segments = path.split('/').filter((part) => part !== '');
  if (segments.length === 0) throw new Error('setPath needs a non-empty path');
  const build = (current: string | null, depth: number): string | null => {
    const name = segments[depth]!;
    const entries: TreeEntry[] =
      current === null ? [] : parseTree(lookup(current)?.bytes ?? Buffer.alloc(0));
    const existing = entries.findIndex((entry) => entry.name === name);
    if (depth === segments.length - 1) {
      if (blobOid === null) {
        if (existing >= 0) entries.splice(existing, 1);
      } else if (existing >= 0) {
        entries[existing] = { mode: '100644', name, oid: blobOid };
      } else {
        entries.push({ mode: '100644', name, oid: blobOid });
      }
    } else {
      const childOid = existing >= 0 && entries[existing]!.mode === '40000' ? entries[existing]!.oid : null;
      const next = build(childOid, depth + 1);
      if (next === null) {
        if (existing >= 0) entries.splice(existing, 1);
      } else if (existing >= 0) {
        entries[existing] = { mode: '40000', name, oid: next };
      } else {
        entries.push({ mode: '40000', name, oid: next });
      }
    }
    if (entries.length === 0) return null;
    return put('tree', buildTree(entries));
  };
  return build(treeOid, 0) ?? put('tree', buildTree([]));
}

/** Flatten a tree into {path: blobOid} for every blob beneath it. */
export function flattenTree(lookup: ObjectLookup, treeOid: string, prefix = ''): Map<string, string> {
  const out = new Map<string, string>();
  const object = lookup(treeOid);
  if (!object || object.kind !== 'tree') return out;
  for (const entry of parseTree(object.bytes)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') {
      for (const [child, oid] of flattenTree(lookup, entry.oid, path)) out.set(child, oid);
    } else {
      out.set(path, entry.oid);
    }
  }
  return out;
}
