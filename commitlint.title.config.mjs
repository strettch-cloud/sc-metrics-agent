// The pull request title gate (.github/workflows/pr-title.yml): the rules of
// commitlint.config.mjs without commitlint's default ignores, which pass
// titles such as "Revert …", "Merge …" or "fixup! …" unchecked. Local commits
// keep those ignores so git revert and fixup commits still work.
import base from "./commitlint.config.mjs";

export default { ...base, defaultIgnores: false };
