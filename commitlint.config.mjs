// Rules for commit messages and pull request titles: Conventional Commits,
// with an optional lowercase scope. The pull request title is the gate
// (.github/workflows/pr-title.yml, through commitlint.title.config.mjs),
// because pull requests are squash-merged and the title becomes the commit on
// main; .githooks/commit-msg gives early feedback. The commit types decide the next version (.github/scripts/next-version.sh).
export default {
  extends: ["@commitlint/config-conventional"],
  rules: {
    "scope-case": [2, "always", "lower-case"],
  },
};
