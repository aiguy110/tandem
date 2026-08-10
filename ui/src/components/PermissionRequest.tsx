interface PermissionRequest {
  actor?: string;
  capability?: string;
  repository?: string;
  tool?: string;
  scope?: string;
  purpose?: string;
  note?: string;
  fallback: string;
}

const TRUSTED_SCRIPTS_NOTICE = 'Scripts are trusted programs with the Tandem process\'s normal host access; this grant controls only brokered MCP calls.';

// ACP permission titles are free-form strings, but Tandem's MCP broker emits
// this stable shape. Keep a graceful fallback for every other agent/tool.
export function parsePermissionRequest(title: string): PermissionRequest {
  const [headline, ...details] = title.split(' · ');
  const parsed: PermissionRequest = { fallback: headline };
  const match = headline.match(/^Allow <(.+?)> to use (.+?) for repository (.+?)\??$/);
  if (match) {
    parsed.actor = match[1];
    parsed.capability = match[2];
    parsed.repository = match[3];
  }

  const detail = details.join(' · ').trim();
  if (!detail) return parsed;

  const scopeMatch = detail.match(/^(.*?)\s+-\s+([A-Za-z][\w -]*):\s*(.*)$/s);
  if (!scopeMatch) {
    parsed.purpose = detail;
    return parsed;
  }

  parsed.tool = scopeMatch[1].trim();
  parsed.scope = scopeMatch[2].trim();
  const explanation = scopeMatch[3].trim();
  if (explanation.endsWith(TRUSTED_SCRIPTS_NOTICE)) {
    parsed.note = TRUSTED_SCRIPTS_NOTICE;
    parsed.purpose = explanation.slice(0, -TRUSTED_SCRIPTS_NOTICE.length).trim();
  } else {
    parsed.purpose = explanation;
  }
  return parsed;
}

export function PermissionRequestDetails({ title }: { title: string }) {
  const request = parsePermissionRequest(title);
  const hasStructuredContent = request.actor || request.tool || request.purpose;

  if (!hasStructuredContent) return <div className="permission-request-fallback">{title}</div>;

  return (
    <div className="permission-request-details">
      <div className="permission-request-title">
        <span className="permission-request-icon" aria-hidden="true">⚠</span>
        {request.actor && request.capability ? (
          <span>Allow <code>{request.actor}</code> to use {request.capability}</span>
        ) : (
          <span>{request.fallback}</span>
        )}
      </div>
      {(request.repository || request.scope) && (
        <div className="permission-request-meta">
          {request.repository && <span><b>Repository</b> {request.repository}</span>}
          {request.scope && <span><b>Scope</b> {request.scope}</span>}
        </div>
      )}
      {request.tool && (
        <div className="permission-request-tool">
          <span>MCP tool</span>
          <code>{request.tool}</code>
        </div>
      )}
      {request.purpose && <p className="permission-request-purpose">{request.purpose}</p>}
      {request.note && <p className="permission-request-note"><span aria-hidden="true">ⓘ</span> {request.note}</p>}
    </div>
  );
}
