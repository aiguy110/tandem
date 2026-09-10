import { useEffect, useState } from 'react';

export type DiffLineKind = 'add' | 'remove' | 'hunk' | 'meta' | 'context';

export interface DiffLine {
  text: string;
  kind: DiffLineKind;
  oldLine?: number;
  newLine?: number;
}

export interface DiffFile {
  key: string;
  label: string;
  lines: DiffLine[];
  isBinary: boolean;
}

const hunkPattern = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

// Kept independent of the workspace pane so ACP edit blocks can feed the same
// unified patches (or synthesized patches) into this renderer.
export function parseUnifiedDiff(patch: string): DiffFile[] {
  const files: DiffFile[] = [];
  let file: DiffFile | undefined;
  let oldLine: number | undefined;
  let newLine: number | undefined;

  for (const text of patch.split('\n')) {
    if (text.startsWith('diff --git ')) {
      const match = /^diff --git a\/(.+) b\/(.+)$/.exec(text);
      file = { key: `${files.length}:${match?.[2] ?? text}`, label: match?.[2] ?? text, lines: [], isBinary: false };
      files.push(file);
    }
    if (!file) {
      file = { key: '0:diff', label: 'Changes', lines: [], isBinary: false };
      files.push(file);
    }
    if (text === 'GIT binary patch' || /^Binary files .+ differ$/.test(text)) {
      file.isBinary = true;
    }

    const hunk = hunkPattern.exec(text);
    let kind: DiffLineKind = 'meta';
    let lineOld: number | undefined;
    let lineNew: number | undefined;
    if (hunk) {
      kind = 'hunk';
      oldLine = Number(hunk[1]);
      newLine = Number(hunk[2]);
    } else if (text.startsWith('+') && !text.startsWith('+++')) {
      kind = 'add';
      lineNew = newLine;
      newLine = (newLine ?? 0) + 1;
    } else if (text.startsWith('-') && !text.startsWith('---')) {
      kind = 'remove';
      lineOld = oldLine;
      oldLine = (oldLine ?? 0) + 1;
    } else if (text.startsWith(' ')) {
      kind = 'context';
      lineOld = oldLine;
      lineNew = newLine;
      oldLine = (oldLine ?? 0) + 1;
      newLine = (newLine ?? 0) + 1;
    }
    file.lines.push({ text, kind, oldLine: lineOld, newLine: lineNew });
  }
  return files;
}

export function UnifiedDiff({ patch, collapseRevision }: { patch: string; collapseRevision?: number }) {
  const files = parseUnifiedDiff(patch);
  const [openFiles, setOpenFiles] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(files.map((file) => [file.key, !file.isBinary])),
  );

  useEffect(() => {
    setOpenFiles(Object.fromEntries(files.map((file) => [file.key, collapseRevision === undefined && !file.isBinary])));
  }, [patch, collapseRevision]);

  return (
    <div className="unified-diff">
      {files.map((file) => (
        <details
          className="diff-file"
          key={file.key}
          open={openFiles[file.key] ?? !file.isBinary}
          onToggle={(event) => setOpenFiles((current) => ({ ...current, [file.key]: event.currentTarget.open }))}
        >
          <summary>
            <span className="diff-file-chevron" aria-hidden="true">›</span>
            <span>{file.label}</span>
            {file.isBinary && <span className="diff-file-kind">binary</span>}
          </summary>
          <div className="diff-code">
            {file.lines.map((line, index) => (
              <div className={`diff-line ${line.kind}`} key={index}>
                <span className="diff-num">{line.oldLine ?? ''}</span>
                <span className="diff-num">{line.newLine ?? ''}</span>
                <span className="diff-text">{line.text || ' '}</span>
              </div>
            ))}
          </div>
        </details>
      ))}
    </div>
  );
}
