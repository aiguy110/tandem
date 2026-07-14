import { useState } from 'react';
import { useStore } from '../store';

// Shown when no token is present, or the daemon rejected it (4401). The user
// pastes the bootstrap token; we persist it and reconnect (D15).
export function TokenScreen({ rejected }: { rejected: boolean }) {
  const submitToken = useStore((s) => s.submitToken);
  const [value, setValue] = useState('');

  const submit = () => {
    const t = value.trim();
    if (t) submitToken(t);
  };

  return (
    <div className="token-screen">
      <div className="token-card">
        <h1>
          <b>Tandem</b> · mission control
        </h1>
        <p>
          Paste the bearer token the daemon printed on startup (the <code>#t=</code> fragment of the bootstrap URL), or open the bootstrap URL
          directly.
        </p>
        <input
          autoFocus
          type="password"
          placeholder="bearer token"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && submit()}
        />
        {rejected && <div className="err">Token rejected by the daemon (4401). Check the value and try again.</div>}
        <button className="btn primary" style={{ marginTop: 12 }} onClick={submit}>
          Connect
        </button>
      </div>
    </div>
  );
}
