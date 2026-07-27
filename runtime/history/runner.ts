import { pathToFileURL } from "node:url";
import { runHistoryImporter, type HistoryImporter } from "./sdk.ts";

async function main(): Promise<void> {
  const [, , parser, ...args] = process.argv;
  if (!parser) {
    throw new Error("usage: runner.ts <absolute-parser-path> [args...]");
  }

  const module = await import(pathToFileURL(parser).href);
  const importer = (module.default ?? module.importer) as HistoryImporter;
  await runHistoryImporter(importer, args);
}

main().catch((error: unknown) => {
  const message = error instanceof Error ? (error.stack ?? error.message) : String(error);
  process.stderr.write(`${message}\n`);
  process.exitCode = 1;
});
