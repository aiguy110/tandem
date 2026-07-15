// SQLite persistence (D14). One file at $TANDEM_HOME/tandem.db in WAL mode.
//
//   agents  — the registry: id, spec JSON, acpSessionId, status, timestamps.
//   events  — per-agent seq-keyed log; PK (agentId, seq). raw_pty payloads are
//             stored base64 inside the JSON payload.
//
// This is both the durable store (survives restart, D11) and the cold-replay
// backstop for the in-memory ring buffer in eventLog.ts.

import Database from 'better-sqlite3';
import type { AgentEvent, AgentRecord, AgentStatus, SpawnSpec } from './types.ts';
import type { LoggedEvent } from './eventLog.ts';

// AgentEvent <-> JSON payload. raw_pty's bytes can't live in JSON, so base64 them.
function encodeEvent(ev: AgentEvent): string {
  if (ev.kind === 'raw_pty') return JSON.stringify({ kind: 'raw_pty', dataB64: Buffer.from(ev.data).toString('base64') });
  return JSON.stringify(ev);
}
function decodeEvent(payload: string): AgentEvent {
  const o = JSON.parse(payload);
  if (o.kind === 'raw_pty') return { kind: 'raw_pty', data: new Uint8Array(Buffer.from(o.dataB64, 'base64')) };
  return o as AgentEvent;
}

export class Db {
  private db: Database.Database;
  private sInsertEvent: Database.Statement;
  private sRangeEvents: Database.Statement;
  private sMaxSeq: Database.Statement;
  private sMinSeq: Database.Statement;
  private sUpsertAgent: Database.Statement;
  private sUpdateStatus: Database.Statement;
  private sUpdateSession: Database.Statement;
  private sCloseAgent: Database.Statement;
  private sLiveAgents: Database.Statement;
  private sGetAgent: Database.Statement;

  constructor(path: string) {
    this.db = new Database(path);
    this.db.pragma('journal_mode = WAL');
    this.db.pragma('synchronous = NORMAL');
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS agents (
        id           TEXT PRIMARY KEY,
        name         TEXT NOT NULL,
        spec         TEXT NOT NULL,
        cwd          TEXT NOT NULL DEFAULT '',
        acpSessionId TEXT,
        status       TEXT NOT NULL,
        createdAt    INTEGER NOT NULL,
        closedAt     INTEGER
      );
      CREATE TABLE IF NOT EXISTS events (
        agentId TEXT NOT NULL,
        seq     INTEGER NOT NULL,
        kind    TEXT NOT NULL,
        payload TEXT NOT NULL,
        ts      INTEGER NOT NULL,
        PRIMARY KEY (agentId, seq)
      );
    `);
    // Phase 2 migration: DBs created before `cwd` existed. No-op on fresh DBs
    // (the CREATE TABLE above already has the column).
    try {
      this.db.exec("ALTER TABLE agents ADD COLUMN cwd TEXT NOT NULL DEFAULT ''");
    } catch {
      /* column already exists */
    }
    this.sInsertEvent = this.db.prepare('INSERT INTO events (agentId, seq, kind, payload, ts) VALUES (?, ?, ?, ?, ?)');
    this.sRangeEvents = this.db.prepare('SELECT seq, payload, ts FROM events WHERE agentId = ? AND seq > ? ORDER BY seq');
    this.sMaxSeq = this.db.prepare('SELECT COALESCE(MAX(seq), 0) AS m FROM events WHERE agentId = ?');
    this.sMinSeq = this.db.prepare('SELECT COALESCE(MIN(seq), 0) AS m FROM events WHERE agentId = ?');
    this.sUpsertAgent = this.db.prepare(
      `INSERT INTO agents (id, name, spec, cwd, acpSessionId, status, createdAt, closedAt)
       VALUES (@id, @name, @spec, @cwd, @acpSessionId, @status, @createdAt, @closedAt)
       ON CONFLICT(id) DO UPDATE SET name=@name, spec=@spec, cwd=@cwd, acpSessionId=@acpSessionId, status=@status, closedAt=@closedAt`,
    );
    this.sUpdateStatus = this.db.prepare('UPDATE agents SET status = ? WHERE id = ?');
    this.sUpdateSession = this.db.prepare('UPDATE agents SET acpSessionId = ? WHERE id = ?');
    this.sCloseAgent = this.db.prepare("UPDATE agents SET status = 'idle', closedAt = ? WHERE id = ?");
    this.sLiveAgents = this.db.prepare('SELECT * FROM agents WHERE closedAt IS NULL ORDER BY createdAt');
    this.sGetAgent = this.db.prepare('SELECT * FROM agents WHERE id = ?');
  }

  // ---- events ----
  appendEvent(agentId: string, le: LoggedEvent): void {
    this.sInsertEvent.run(agentId, le.seq, le.event.kind, encodeEvent(le.event), le.ts);
  }
  /** Persisted events with seq > afterSeq (cold-replay backstop). */
  rangeEvents(agentId: string, afterSeq: number): LoggedEvent[] {
    return (this.sRangeEvents.all(agentId, afterSeq) as { seq: number; payload: string; ts: number }[]).map((r) => ({
      seq: r.seq,
      event: decodeEvent(r.payload),
      ts: r.ts,
    }));
  }
  maxSeq(agentId: string): number {
    return (this.sMaxSeq.get(agentId) as { m: number }).m;
  }
  minSeq(agentId: string): number {
    return (this.sMinSeq.get(agentId) as { m: number }).m;
  }

  // ---- agents ----
  upsertAgent(rec: AgentRecord): void {
    this.sUpsertAgent.run({
      id: rec.id,
      name: rec.name,
      spec: JSON.stringify(rec.spec),
      cwd: rec.cwd,
      acpSessionId: rec.acpSessionId,
      status: rec.status,
      createdAt: rec.createdAt,
      closedAt: rec.closedAt,
    });
  }
  setStatus(id: string, status: AgentStatus): void {
    this.sUpdateStatus.run(status, id);
  }
  setSessionId(id: string, sessionId: string): void {
    this.sUpdateSession.run(sessionId, id);
  }
  closeAgent(id: string): void {
    this.sCloseAgent.run(Date.now(), id);
  }
  liveAgents(): AgentRecord[] {
    return (this.sLiveAgents.all() as any[]).map(rowToRecord);
  }
  // Highest `-N` suffix among ALL agent ids ever created (including closed
  // ones still in the DB). Names/ids double as the stable key, so the
  // auto-name counter must never regenerate one already used — seeding it
  // from live-agent count alone let closed agents' ids get reissued to a
  // fresh agent, silently splicing the old agent's persisted event-log
  // history onto the new one.
  maxAgentSuffix(): number {
    const rows = this.db.prepare('SELECT id FROM agents').all() as { id: string }[];
    let max = 0;
    for (const { id } of rows) {
      const m = /-(\d+)$/.exec(id);
      if (m) max = Math.max(max, parseInt(m[1], 10));
    }
    return max;
  }
  getAgent(id: string): AgentRecord | undefined {
    const r = this.sGetAgent.get(id) as any;
    return r ? rowToRecord(r) : undefined;
  }

  close(): void {
    // Fold the WAL back into the main db so a fresh process sees a clean file.
    try {
      this.db.pragma('wal_checkpoint(TRUNCATE)');
    } catch {
      /* best-effort */
    }
    this.db.close();
  }
}

function rowToRecord(r: any): AgentRecord {
  return {
    id: r.id,
    name: r.name,
    spec: JSON.parse(r.spec) as SpawnSpec,
    cwd: r.cwd ?? '',
    acpSessionId: r.acpSessionId ?? null,
    status: r.status as AgentStatus,
    createdAt: r.createdAt,
    closedAt: r.closedAt ?? null,
  };
}
