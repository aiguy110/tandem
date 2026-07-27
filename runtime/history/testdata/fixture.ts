import { defineHistoryImporter } from "@tandem/history-importer";

export default defineHistoryImporter({
  id: "sdk-fixture",
  version: 2,
  async scan(ctx) {
    if (ctx.args[0] !== "--fixture") throw new Error("arguments were not forwarded");
    if ((ctx.checkpoints.get("source") as { offset: number })?.offset !== 7) {
      throw new Error("matching checkpoint was not provided");
    }
    await ctx.session({
      sourceKey: "source",
      session: { id: "session", title: "SDK fixture" },
      entries: [{ id: "entry", ordinal: 1, role: "user", text: "hello history" }],
      checkpoint: { offset: 8 },
    });
  },
});
