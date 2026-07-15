// Transcript image-upload regression suite. Exercises the public HTTP + WS
// surfaces and uses the mock ACP agent's deterministic prompt echo to verify
// the final ACP ContentBlock ordering and bytes, rather than testing internal
// helpers in isolation.

import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { WebSocket } from 'ws';
import { makeHarness, open, report, sleep, type Frame, type Harness } from './testHarness.ts';

const PORT = 7761;
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
  'base64',
);

class Client {
  frames: Frame[] = [];
  constructor(public ws: WebSocket) {}
  track(frame: Frame): void {
    this.frames.push(frame);
  }
  send(message: unknown): void {
    this.ws.send(JSON.stringify(message));
  }
  allEvents(agentId: string): any[] {
    const events: any[] = [];
    for (const frame of this.frames) {
      if (frame.agentId !== agentId) continue;
      if (frame.t === 'snapshot') events.push(...(frame.transcript ?? []).map((item) => item.event));
      if (frame.t === 'event' && frame.event) events.push(frame.event);
    }
    return events;
  }
  ack(corrId: string): Frame | undefined {
    return this.frames.find((frame) => frame.t === 'ack' && frame.corrId === corrId);
  }
}

async function waitFor<T>(read: () => T | undefined, timeoutMs = 4_000): Promise<T | undefined> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const value = read();
    if (value !== undefined) return value;
    await sleep(25);
  }
  return undefined;
}

async function spawn(h: Harness, cwd: string): Promise<{ client: Client; agentId: string }> {
  let client!: Client;
  client = new Client(await open(h.port, h.token, (frame) => client.track(frame)));
  client.send({
    t: 'spawn_agent',
    corrId: 'spawn',
    spec: { adapter: 'acp', workspace: { kind: 'existing', cwd } },
  });
  const ack = await waitFor(() => client.ack('spawn'));
  if (!ack?.agentId) throw new Error(`agent spawn failed: ${ack?.error ?? 'no ack'}`);
  client.send({ t: 'subscribe', corrId: 'sub', agentId: ack.agentId, channels: ['transcript', 'status'], sinceSeq: 0 });
  await waitFor(() => client.ack('sub'));
  return { client, agentId: ack.agentId };
}

async function upload(
  h: Harness,
  agentId: string,
  bytes: Uint8Array,
  opts: { token?: string | null; mime?: string; name?: string } = {},
): Promise<{ status: number; body: any }> {
  const headers: Record<string, string> = {
    'content-type': opts.mime ?? 'image/png',
    'x-file-name': opts.name ?? 'pixel.png',
  };
  if (opts.token !== null) headers.authorization = `Bearer ${opts.token ?? h.token}`;
  const response = await fetch(`http://127.0.0.1:${h.port}/api/agents/${encodeURIComponent(agentId)}/assets`, {
    method: 'POST',
    headers,
    body: Buffer.from(bytes),
  });
  const text = await response.text();
  let body: any = text;
  try { body = JSON.parse(text); } catch { /* status assertions retain text */ }
  return { status: response.status, body };
}

async function main(): Promise<void> {
  const checks: [string, boolean, string][] = [];
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-images-cwd-'));
  const otherCwd = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-images-other-'));
  process.env.TANDEM_MOCK_IMAGE_CAPABILITY = 'true';
  const h = await makeHarness(PORT);
  let client: Client | undefined;

  try {
    const spawned = await spawn(h, cwd);
    client = spawned.client;
    const { agentId } = spawned;

    const noAuth = await upload(h, agentId, PNG, { token: null });
    const badMime = await upload(h, agentId, Buffer.from('<svg/>'), { mime: 'image/svg+xml', name: 'active.svg' });
    const mismatched = await upload(h, agentId, PNG, { mime: 'image/jpeg', name: 'lie.jpg' });
    const pixelBomb = Buffer.from(PNG);
    pixelBomb.writeUInt32BE(20_000, 16);
    pixelBomb.writeUInt32BE(20_000, 20);
    const unsafeDimensions = await upload(h, agentId, pixelBomb, { name: 'pixel-bomb.png' });
    checks.push([
      'upload auth, type/signature, and decoded-size validation',
      noAuth.status === 401 && badMime.status === 415 && mismatched.status === 415 && unsafeDimensions.status === 415,
      `noAuth=${noAuth.status}, svg=${badMime.status}, mismatch=${mismatched.status}, dimensions=${unsafeDimensions.status}`,
    ]);

    const first = await upload(h, agentId, PNG);
    const duplicate = await upload(h, agentId, PNG, { name: 'same-bytes-different-name.png' });
    const assetId = first.body?.asset?.assetId as string | undefined;
    checks.push([
      'uploads are content-addressed and idempotent',
      first.status === 201 && duplicate.status === 201 && !!assetId && duplicate.body?.asset?.assetId === assetId,
      `first=${first.status}, duplicate=${duplicate.status}, sameId=${duplicate.body?.asset?.assetId === assetId}`,
    ]);

    const getNoAuth = await fetch(`http://127.0.0.1:${PORT}/api/agents/${agentId}/assets/${assetId}`);
    const get = await fetch(`http://127.0.0.1:${PORT}/api/agents/${agentId}/assets/${assetId}`, {
      headers: { authorization: `Bearer ${h.token}` },
    });
    const downloaded = Buffer.from(await get.arrayBuffer());
    checks.push([
      'asset retrieval is authenticated and byte-exact',
      getNoAuth.status === 401 && get.status === 200 && get.headers.get('content-type') === 'image/png' && downloaded.equals(PNG),
      `noAuth=${getNoAuth.status}, get=${get.status}, mime=${get.headers.get('content-type')}, bytes=${downloaded.length}`,
    ]);

    const other = await spawn(h, otherCwd);
    const crossAgent = await fetch(`http://127.0.0.1:${PORT}/api/agents/${other.agentId}/assets/${assetId}`, {
      headers: { authorization: `Bearer ${h.token}` },
    });
    checks.push([
      'asset ownership is scoped to the uploading agent',
      crossAgent.status === 404,
      `crossAgentGet=${crossAgent.status}`,
    ]);
    other.client.ws.close();

    const tooLarge = Buffer.alloc(10 * 1024 * 1024 + 1);
    PNG.subarray(0, 8).copy(tooLarge);
    const overLimit = await upload(h, agentId, tooLarge, { name: 'too-large.png' });
    checks.push(['per-image byte limit is enforced', overLimit.status === 413, `status=${overLimit.status}`]);

    client.send({
      t: 'prompt',
      corrId: 'image-prompt',
      agentId,
      blocks: [
        { type: 'text', text: 'DERISK_IMAGE before' },
        { type: 'image', assetId, mimeType: 'image/png', name: 'pixel.png' },
        { type: 'text', text: 'after' },
      ],
    });
    const promptAck = await waitFor(() => client!.ack('image-prompt'));
    const digest = crypto.createHash('sha256').update(PNG).digest('hex').slice(0, 16);
    const expectedEcho = `ACP_PROMPT_BLOCKS text:DERISK_IMAGE before|image:image/png:${PNG.length}:${digest}|text:after`;
    const echo = await waitFor(() => client!.allEvents(agentId).find((event) => event.kind === 'message_chunk' && event.text === expectedEcho));
    checks.push([
      'mixed blocks reach ACP in order with original image bytes',
      !promptAck?.error && !!echo,
      `ackError=${promptAck?.error ?? 'none'}, echo=${!!echo}`,
    ]);

    const transcriptEvents = client.allEvents(agentId);
    const userMessage = transcriptEvents.find((event) => event.kind === 'user_message' && Array.isArray(event.blocks));
    const capability = transcriptEvents.find((event) => event.kind === 'prompt_capabilities');
    checks.push([
      'capability and durable user image blocks are transcript events',
      capability?.image === true && userMessage?.blocks?.[1]?.assetId === assetId && userMessage?.blocks?.[2]?.text === 'after',
      `imageCapability=${capability?.image}, userBlocks=${userMessage?.blocks?.length ?? 0}`,
    ]);

    // A fresh socket reconstructs the image prompt from the daemon event log.
    client.ws.terminate();
    let replay!: Client;
    replay = new Client(await open(PORT, h.token, (frame) => replay.track(frame)));
    replay.send({ t: 'subscribe', corrId: 'replay', agentId, channels: ['transcript'], sinceSeq: 0 });
    await waitFor(() => replay.ack('replay'));
    const replayed = replay.allEvents(agentId).find((event) => event.kind === 'user_message' && event.blocks?.[1]?.assetId === assetId);
    checks.push(['image prompt survives durable transcript replay', !!replayed, `replayed=${!!replayed}`]);
    replay.ws.close();

    // Four references to one 6 MiB asset avoid wasting 24 MiB on disk while
    // proving that the 20 MiB aggregate turn limit counts logical attachments.
    // Preserve a complete valid image and add inert trailing bytes; image
    // decoders permit data after PNG's IEND chunk and the upload validator can
    // still inspect real dimensions.
    const sixMiB = Buffer.concat([PNG, Buffer.alloc(6 * 1024 * 1024 - PNG.length)]);
    const large = await upload(h, agentId, sixMiB, { name: 'six-mib.png' });
    const repeated = Array.from({ length: 4 }, () => ({ type: 'image', assetId: large.body?.asset?.assetId, mimeType: 'image/png' }));
    let limitsClient!: Client;
    limitsClient = new Client(await open(PORT, h.token, (frame) => limitsClient.track(frame)));
    limitsClient.send({ t: 'prompt', corrId: 'total-limit', agentId, blocks: [{ type: 'text', text: 'DERISK_IMAGE' }, ...repeated] });
    const totalAck = await waitFor(() => limitsClient.ack('total-limit'));
    limitsClient.send({ t: 'prompt', corrId: 'count-limit', agentId, blocks: [{ type: 'text', text: 'DERISK_IMAGE' }, ...repeated, repeated[0]] });
    const countAck = await waitFor(() => limitsClient.ack('count-limit'));
    checks.push([
      'per-turn aggregate bytes and image count are enforced',
      large.status === 201 && String(totalAck?.error).includes('byte limit') && String(countAck?.error).includes('at most 4'),
      `upload=${large.status} body=${JSON.stringify(large.body).slice(0, 160)}, total=${totalAck?.error ?? 'accepted'}, count=${countAck?.error ?? 'accepted'}`,
    ]);
    limitsClient.ws.close();
  } finally {
    client?.ws.close();
    await h.stop();
  }

  // An independently initialized adapter that explicitly advertises image=false
  // must reject before appending user_message or calling session/prompt.
  process.env.TANDEM_MOCK_IMAGE_CAPABILITY = 'false';
  const h2 = await makeHarness(PORT + 1);
  try {
    const incapable = await spawn(h2, cwd);
    const uploaded = await upload(h2, incapable.agentId, PNG);
    incapable.client.send({
      t: 'prompt', corrId: 'unsupported', agentId: incapable.agentId,
      blocks: [{ type: 'text', text: 'DERISK_IMAGE unsupported' }, { type: 'image', assetId: uploaded.body?.asset?.assetId, mimeType: 'image/png' }],
    });
    const ack = await waitFor(() => incapable.client.ack('unsupported'));
    await sleep(200);
    const events = incapable.client.allEvents(incapable.agentId);
    const capability = events.find((event) => event.kind === 'prompt_capabilities');
    const logged = events.some((event) => event.kind === 'user_message' && event.blocks?.some((block: any) => block.type === 'image'));
    checks.push([
      'image-incapable agents reject before starting or logging a turn',
      capability?.image === false && !!ack?.error && !logged,
      `imageCapability=${capability?.image}, error=${ack?.error ?? 'none'}, logged=${logged}`,
    ]);
    incapable.client.ws.close();
  } finally {
    await h2.stop();
    fs.rmSync(cwd, { recursive: true, force: true });
    fs.rmSync(otherCwd, { recursive: true, force: true });
    delete process.env.TANDEM_MOCK_IMAGE_CAPABILITY;
  }

  console.log('\n' + '─'.repeat(64));
  console.log('  TANDEM · transcript image uploads');
  console.log('─'.repeat(64));
  process.exit(report(checks) ? 0 : 1);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
