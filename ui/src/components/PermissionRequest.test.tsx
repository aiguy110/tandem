import { describe, expect, it } from 'vitest';
import { parsePermissionRequest } from './PermissionRequest';

describe('parsePermissionRequest', () => {
  it('separates Tandem MCP broker permission details into display fields', () => {
    const request = parsePermissionRequest(
      "Allow <evaluate> to use Tandem MCP tools for repository einstein-1? · mcp__claude_ai_Microsoft_365__outlook_email_search - One-off: page the latest 1000 inbox messages and report which are unread (isRead == false), since the server-side search cannot filter by read state. Scripts are trusted programs with the Tandem process's normal host access; this grant controls only brokered MCP calls.",
    );

    expect(request).toMatchObject({
      actor: 'evaluate',
      capability: 'Tandem MCP tools',
      repository: 'einstein-1',
      tool: 'mcp__claude_ai_Microsoft_365__outlook_email_search',
      scope: 'One-off',
      purpose: 'page the latest 1000 inbox messages and report which are unread (isRead == false), since the server-side search cannot filter by read state.',
    });
    expect(request.note).toContain('trusted programs');
  });

  it('preserves unfamiliar permission titles as a fallback', () => {
    expect(parsePermissionRequest('Run deploy command')).toEqual({ fallback: 'Run deploy command' });
  });
});
