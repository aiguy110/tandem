import { homePath } from "./common.ts";
import { piSessionImporter } from "./pi.ts";

// oh-my-pi keeps Pi's session format under its own agent directory.
export default piSessionImporter({
  id: "omp",
  defaultRoot: homePath(".omp", "agent", "sessions"),
  skipNestedSessions: true,
});
