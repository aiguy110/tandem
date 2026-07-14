// Diff pane — the workspace's uncommitted diff, reviewable/stageable. The wire
// side (merge_back) still returns an error ack; the diff/merge UI is a later
// phase. Placeholder for now.
export function DiffPane() {
  return (
    <div className="pane">
      <div className="pane-placeholder">
        <div className="big">±</div>
        <div>
          <b>Diff</b> — the agent’s uncommitted worktree diff, reviewable and mergeable.
          <br />
          Coming in a later phase (the daemon’s <code>merge_back</code> is still stubbed).
        </div>
      </div>
    </div>
  );
}
