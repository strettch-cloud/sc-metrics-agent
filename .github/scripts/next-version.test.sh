#!/usr/bin/env bash
# Tests for next-version.sh. Each test builds a throwaway git repository with
# its own commits and tags, runs the script once, and checks what it printed,
# its exit code and the run summary.
# Run: .github/scripts/next-version.test.sh
set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/next-version.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A GitHub Actions runner sets this for the test job itself; every run below
# sets or removes it explicitly.
unset GITHUB_STEP_SUMMARY

failures=0
test_count=0

# --- helpers (run inside each test's subshell) -------------------------------

fail() {
  echo "    $*" >&2
  return 1
}

# Creates an empty repository and moves into it. The local settings keep the
# machine's global git config (signing, hooks) out of the test.
new_repo() {
  test_dir="$(mktemp -d "$WORK/test.XXXXXX")"
  repo="$test_dir/repo"
  stdout_file="$test_dir/stdout"
  stderr_file="$test_dir/stderr"
  summary_file="$test_dir/summary.md"
  mkdir "$repo"
  cd "$repo"
  git init -q
  git config user.name "Release Test"
  git config user.email "release-test@example.invalid"
  git config commit.gpgsign false
  git config tag.gpgsign false
  git config core.hooksPath /dev/null
  commit_clock=1700000000
}

# commit "<subject>" ["<body>"]: an empty commit, one minute after the last.
commit() {
  commit_clock=$((commit_clock + 60))
  local when="@$commit_clock +0000"
  if [ "$#" -ge 2 ]; then
    GIT_AUTHOR_DATE="$when" GIT_COMMITTER_DATE="$when" \
      git commit -q --allow-empty -m "$1" -m "$2"
  else
    GIT_AUTHOR_DATE="$when" GIT_COMMITTER_DATE="$when" \
      git commit -q --allow-empty -m "$1"
  fi
}

# tag "<name>": a lightweight tag on the current commit.
tag() {
  git tag "$1"
}

# Runs the script with the run summary file set; fills exit_code.
run_release() {
  exit_code=0
  GITHUB_STEP_SUMMARY="$summary_file" "$SCRIPT" "$@" \
    >"$stdout_file" 2>"$stderr_file" || exit_code=$?
}

# Runs the script with no run summary file; fills exit_code.
run_release_without_summary() {
  exit_code=0
  env -u GITHUB_STEP_SUMMARY "$SCRIPT" "$@" \
    >"$stdout_file" 2>"$stderr_file" || exit_code=$?
}

expect_version() {
  local expected="$1" printed lines
  printed="$(cat "$stdout_file")"
  lines="$(wc -l <"$stdout_file" | tr -d ' ')"
  [ "$exit_code" -eq 0 ] ||
    fail "expected exit 0, got $exit_code; stderr: $(cat "$stderr_file")"
  [ "$printed" = "$expected" ] ||
    fail "expected version '$expected', stdout was '$printed'"
  [ "$lines" = "1" ] || fail "expected one line on stdout, got $lines"
}

expect_nothing_to_release() {
  [ "$exit_code" -eq 3 ] ||
    fail "expected exit 3, got $exit_code; stdout: $(cat "$stdout_file")"
  [ ! -s "$stdout_file" ] ||
    fail "expected empty stdout, got '$(cat "$stdout_file")'"
  grep -q "nothing to release" "$stderr_file" ||
    fail "expected 'nothing to release' on stderr, got '$(cat "$stderr_file")'"
}

expect_no_stable_release_tag() {
  [ "$exit_code" -eq 1 ] ||
    fail "expected exit 1, got $exit_code; stdout: $(cat "$stdout_file")"
  [ ! -s "$stdout_file" ] ||
    fail "expected empty stdout, got '$(cat "$stdout_file")'"
  grep -q "no stable release tag found" "$stderr_file" ||
    fail "expected 'no stable release tag found' on stderr, got '$(cat "$stderr_file")'"
}

expect_usage_error() {
  [ "$exit_code" -eq 2 ] || fail "expected exit 2, got $exit_code"
  [ ! -s "$stdout_file" ] ||
    fail "expected empty stdout, got '$(cat "$stdout_file")'"
}

expect_in_file() {
  local file="$1" text="$2" label="$3"
  [ -f "$file" ] || fail "$label was not written"
  grep -qF -- "$text" "$file" ||
    fail "expected '$text' in $label, got '$(cat "$file")'"
}

expect_not_in_file() {
  local file="$1" text="$2" label="$3"
  if [ -f "$file" ] && grep -qF -- "$text" "$file"; then
    fail "did not expect '$text' in $label, got '$(cat "$file")'"
  fi
}

# --- tests -------------------------------------------------------------------
# Tag names are the releases already out; commit subjects are the commits on
# main after them.

# Flow: the repository has no stable release tag

test_beta_with_no_stable_release_tag() {
  new_repo
  commit "feat: report disk usage"

  run_release beta

  expect_no_stable_release_tag
}

test_stable_with_no_stable_release_tag() {
  new_repo
  commit "fix: round disk usage"

  run_release stable

  expect_no_stable_release_tag
}

test_stable_major_with_no_stable_release_tag() {
  new_repo
  commit "fix: round disk usage"

  run_release stable --major

  expect_no_stable_release_tag
}

test_only_v_prefixed_tags_is_no_stable_release_tag() {
  new_repo
  commit "feat: report disk usage"
  tag "v0.3.1"
  commit "fix: skip loop devices"

  run_release stable

  expect_no_stable_release_tag
}

test_only_beta_tags_is_no_stable_release_tag() {
  new_repo
  commit "feat: report disk usage"
  tag "0.1.0-beta.1"
  commit "fix: skip loop devices"

  run_release beta

  expect_no_stable_release_tag
}

# Flow: no commits since the last stable release

test_beta_with_no_commits_since_stable() {
  new_repo
  commit "feat: report disk usage"
  tag "0.4.0-beta.1"
  commit "fix: round disk usage"
  tag "0.4.0-beta.2"
  tag "0.4.0"

  run_release beta

  expect_nothing_to_release
}

test_stable_with_no_commits_since_stable() {
  new_repo
  commit "feat: report disk usage"
  tag "0.4.0"

  run_release stable

  expect_nothing_to_release
}

test_stable_major_with_no_commits_since_stable() {
  new_repo
  commit "feat: report disk usage"
  tag "0.4.0"

  run_release stable --major

  expect_nothing_to_release
}

# Flow: stable release with "major" ticked

test_stable_major_after_0_3_1() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "feat: report network usage"

  run_release stable --major

  expect_version "1.0.0"
}

# Flow: a feat since the last stable release raises the minor number

test_beta_feat_after_0_3_1() {
  new_repo
  commit "fix: round disk usage"
  tag "0.3.1"
  commit "feat: report network usage"

  run_release beta

  expect_version "0.4.0-beta.1"
}

test_beta_feat_after_fix_beta() {
  new_repo
  commit "fix: round disk usage"
  tag "0.3.1"
  commit "fix: skip read-only mounts"
  tag "0.3.2-beta.1"
  commit "fix: skip loop devices"
  tag "0.3.2-beta.2"
  commit "feat: report network usage"

  run_release beta

  expect_version "0.4.0-beta.1"
}

test_beta_fix_after_feat_beta() {
  new_repo
  commit "fix: round disk usage"
  tag "0.3.1"
  commit "feat: report network usage"
  tag "0.4.0-beta.1"
  commit "fix: skip loop devices"

  run_release beta

  expect_version "0.4.0-beta.2"
}

test_beta_feat_after_two_minor_betas() {
  new_repo
  commit "fix: round disk usage"
  tag "0.3.1"
  commit "feat: report network usage"
  tag "0.4.0-beta.1"
  commit "fix: skip loop devices"
  tag "0.4.0-beta.2"
  commit "feat: report memory usage"

  run_release beta

  expect_version "0.4.0-beta.3"
}

test_stable_feat_after_0_3_1() {
  new_repo
  commit "fix: round disk usage"
  tag "0.3.1"
  commit "feat: report network usage"

  run_release stable

  expect_version "0.4.0"
}

test_stable_feat_ignores_betas_already_out() {
  new_repo
  commit "fix: round disk usage"
  tag "0.3.1"
  commit "feat: report network usage"
  tag "0.4.0-beta.1"
  commit "fix: skip loop devices"
  tag "0.4.0-beta.2"

  run_release stable

  expect_version "0.4.0"
}

test_scoped_feat_raises_minor() {
  new_repo
  commit "fix: round disk usage"
  tag "0.3.1"
  commit "feat(disk): report inode usage"

  run_release stable

  expect_version "0.4.0"
}

# Flow: no feat since the last stable release raises the patch number

test_beta_fix_after_0_3_1() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: skip loop devices"

  run_release beta

  expect_version "0.3.2-beta.1"
}

test_beta_fix_after_two_patch_betas() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: skip loop devices"
  tag "0.3.2-beta.1"
  commit "fix: skip read-only mounts"
  tag "0.3.2-beta.2"
  commit "fix: round network usage"

  run_release beta

  expect_version "0.3.2-beta.3"
}

test_beta_rerun_on_commit_with_a_beta() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: skip loop devices"
  tag "0.3.2-beta.1"
  commit "fix: skip read-only mounts"
  tag "0.3.2-beta.2"

  run_release beta

  expect_version "0.3.2-beta.3"
}

test_stable_fix_after_0_3_1() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: skip loop devices"

  run_release stable

  expect_version "0.3.2"
}

test_beta_fix_after_stable_cut_from_betas() {
  new_repo
  commit "feat: report disk usage"
  tag "0.4.0-beta.1"
  commit "fix: round disk usage"
  tag "0.4.0-beta.2"
  tag "0.4.0"
  commit "fix: skip loop devices"

  run_release beta

  expect_version "0.4.1-beta.1"
}

test_chore_docs_ci_raise_patch() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "chore: bump dependencies"
  commit "docs: describe the config file"
  commit "ci: cache the build"

  run_release stable

  expect_version "0.3.2"
}

test_non_conventional_subject_raises_patch() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "Update README"

  run_release stable

  expect_version "0.3.2"
}

test_fix_subject_mentioning_feat_raises_patch() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: undo feat: report disk usage on read-only mounts"

  run_release stable

  expect_version "0.3.2"
}

# Flow: breaking commits are listed and never raise the major number

test_breaking_fix_stable_version() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix!: report disk usage in bytes"

  run_release stable

  expect_version "0.3.2"
}

test_breaking_fix_stable_is_listed() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix!: report disk usage in bytes"

  run_release stable

  expect_in_file "$stderr_file" "fix!: report disk usage in bytes" "stderr"
  expect_in_file "$summary_file" "fix!: report disk usage in bytes" "the run summary"
}

test_breaking_feat_stable_version() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "feat!: rename the config file"

  run_release stable

  expect_version "0.4.0"
}

test_breaking_feat_stable_is_listed() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "feat!: rename the config file"

  run_release stable

  expect_in_file "$stderr_file" "feat!: rename the config file" "stderr"
  expect_in_file "$summary_file" "feat!: rename the config file" "the run summary"
}

test_breaking_scoped_feat_is_listed_and_raises_minor() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "feat(config)!: rename the config file"

  run_release stable

  expect_version "0.4.0"
  expect_in_file "$summary_file" "feat(config)!: rename the config file" "the run summary"
}

test_breaking_change_footer_is_listed_and_raises_patch() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: report disk usage in bytes" \
    "BREAKING CHANGE: dashboards reading kilobytes must convert"

  run_release stable

  expect_version "0.3.2"
  expect_in_file "$summary_file" "fix: report disk usage in bytes" "the run summary"
}

test_breaking_change_hyphen_footer_is_listed() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: report disk usage in bytes" \
    "BREAKING-CHANGE: dashboards reading kilobytes must convert"

  run_release stable

  expect_version "0.3.2"
  expect_in_file "$summary_file" "fix: report disk usage in bytes" "the run summary"
}

test_breaking_feat_beta_is_listed() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "feat!: rename the config file"

  run_release beta

  expect_version "0.4.0-beta.1"
  expect_in_file "$summary_file" "feat!: rename the config file" "the run summary"
}

test_breaking_commit_before_last_stable_is_not_listed() {
  new_repo
  commit "feat!: rename the config file"
  tag "1.0.0"
  commit "fix: skip loop devices"

  run_release stable

  expect_version "1.0.1"
  expect_not_in_file "$summary_file" "feat!: rename the config file" "the run summary"
  expect_not_in_file "$stderr_file" "feat!: rename the config file" "stderr"
}

test_non_breaking_commit_is_not_listed() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix!: report disk usage in bytes"
  commit "fix: skip loop devices"

  run_release stable

  expect_in_file "$summary_file" "fix!: report disk usage in bytes" "the run summary"
  expect_not_in_file "$summary_file" "fix: skip loop devices" "the run summary"
}

test_breaking_commit_listed_without_summary_file() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix!: report disk usage in bytes"

  run_release_without_summary beta

  expect_version "0.3.2-beta.1"
  expect_in_file "$stderr_file" "fix!: report disk usage in bytes" "stderr"
}

test_summary_is_appended_not_replaced() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix!: report disk usage in bytes"
  earlier_step_line="Built the packages"
  echo "$earlier_step_line" >"$summary_file"

  run_release stable

  expect_in_file "$summary_file" "$earlier_step_line" "the run summary"
  expect_in_file "$summary_file" "fix!: report disk usage in bytes" "the run summary"
}

# Which tags count

# 0.11.0 is neither first nor last in alphabetical order, and it is on the
# oldest commit, so only version order picks it.
test_highest_stable_by_version_order() {
  new_repo
  commit "feat: report disk usage"
  tag "0.11.0"
  commit "fix: backport to an older line"
  tag "0.10.0"
  commit "fix: backport to the oldest line"
  tag "0.9.0"
  commit "fix: skip loop devices"

  run_release stable

  expect_version "0.11.1"
}

test_highest_beta_by_version_order() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "feat: report network usage"
  tag "0.4.0-beta.10"
  tag "0.4.0-beta.9"
  commit "fix: skip loop devices"

  run_release beta

  expect_version "0.4.0-beta.11"
}

test_betas_of_another_version_are_ignored() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: skip loop devices"
  tag "0.5.0-beta.3"
  commit "feat: report network usage"

  run_release beta

  expect_version "0.4.0-beta.1"
}

test_v_prefixed_tag_is_not_a_stable_release() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: skip loop devices"
  tag "v9.9.9"

  run_release stable

  expect_version "0.3.2"
}

test_runs_from_a_subdirectory() {
  new_repo
  commit "feat: report disk usage"
  tag "0.3.1"
  commit "fix: skip loop devices"
  mkdir -p deb/control
  cd deb/control

  run_release stable

  expect_version "0.3.2"
}

# Bad usage

test_unknown_kind_is_bad_usage() {
  new_repo
  commit "feat: report disk usage"

  run_release nightly

  expect_usage_error
}

test_major_with_beta_is_bad_usage() {
  new_repo
  commit "feat: report disk usage"

  run_release beta --major

  expect_usage_error
}

# --- runner ------------------------------------------------------------------

run_test() {
  local sentence="$1" fn="$2" status
  test_count=$((test_count + 1))
  set +e
  (set -e; "$fn")
  status=$?
  set -e
  if [ "$status" -eq 0 ]; then
    echo "pass  $sentence"
  else
    echo "FAIL  $sentence"
    failures=$((failures + 1))
  fi
}

if [ ! -f "$SCRIPT" ]; then
  echo "next-version.sh not found at $SCRIPT" >&2
  exit 1
fi

run_test "a beta run in a repository with no stable release tag fails with no stable release tag found" \
  test_beta_with_no_stable_release_tag
run_test "a stable run in a repository with no stable release tag fails with no stable release tag found" \
  test_stable_with_no_stable_release_tag
run_test "a stable run with major ticked in a repository with no stable release tag fails with no stable release tag found" \
  test_stable_major_with_no_stable_release_tag
run_test "a repository with only v-prefixed tags has no stable release tag" \
  test_only_v_prefixed_tags_is_no_stable_release_tag
run_test "a repository with only beta tags has no stable release tag" \
  test_only_beta_tags_is_no_stable_release_tag
run_test "a beta run with no commits since stable 0.4.0, cut on the commit of 0.4.0-beta.2, makes no beta and exits 3" \
  test_beta_with_no_commits_since_stable
run_test "a stable run with no commits since stable 0.4.0 fails with nothing to release" \
  test_stable_with_no_commits_since_stable
run_test "a stable run with major ticked and no commits since stable 0.4.0 fails with nothing to release" \
  test_stable_major_with_no_commits_since_stable
run_test "a stable run with major ticked after 0.3.1 releases 1.0.0" \
  test_stable_major_after_0_3_1
run_test "a beta run with a feat after 0.3.1 releases 0.4.0-beta.1" \
  test_beta_feat_after_0_3_1
run_test "a beta run with a feat after 0.3.2-beta.2 releases 0.4.0-beta.1" \
  test_beta_feat_after_fix_beta
run_test "a beta run with a fix after 0.4.0-beta.1 releases 0.4.0-beta.2" \
  test_beta_fix_after_feat_beta
run_test "a beta run with a feat after 0.4.0-beta.2 releases 0.4.0-beta.3" \
  test_beta_feat_after_two_minor_betas
run_test "a stable run with a feat after 0.3.1 releases 0.4.0" \
  test_stable_feat_after_0_3_1
run_test "a stable run with a feat after 0.3.1 releases 0.4.0 even when 0.4.0 betas are out" \
  test_stable_feat_ignores_betas_already_out
run_test "a feat with a scope after 0.3.1 raises the minor number to 0.4.0" \
  test_scoped_feat_raises_minor
run_test "a beta run with a fix after 0.3.1 releases 0.3.2-beta.1" \
  test_beta_fix_after_0_3_1
run_test "a beta run with a fix after 0.3.2-beta.2 releases 0.3.2-beta.3" \
  test_beta_fix_after_two_patch_betas
run_test "a beta run again on the commit of 0.3.2-beta.2 releases the same code as 0.3.2-beta.3" \
  test_beta_rerun_on_commit_with_a_beta
run_test "a stable run with a fix after 0.3.1 releases 0.3.2" \
  test_stable_fix_after_0_3_1
run_test "a beta run with a fix after stable 0.4.0 and its betas releases 0.4.1-beta.1" \
  test_beta_fix_after_stable_cut_from_betas
run_test "chore, docs and ci commits after 0.3.1 raise the patch number to 0.3.2" \
  test_chore_docs_ci_raise_patch
run_test "a subject that is not a Conventional Commit after 0.3.1 raises the patch number to 0.3.2" \
  test_non_conventional_subject_raises_patch
run_test "a fix whose subject mentions feat: after 0.3.1 raises the patch number to 0.3.2" \
  test_fix_subject_mentioning_feat_raises_patch
run_test "a stable run with a breaking fix after 0.3.1 releases 0.3.2, not a new major" \
  test_breaking_fix_stable_version
run_test "a stable run lists a breaking fix on stderr and in the run summary" \
  test_breaking_fix_stable_is_listed
run_test "a stable run with a breaking feat after 0.3.1 releases 0.4.0, not a new major" \
  test_breaking_feat_stable_version
run_test "a stable run lists a breaking feat on stderr and in the run summary" \
  test_breaking_feat_stable_is_listed
run_test "a breaking feat with a scope raises the minor number to 0.4.0 and is listed" \
  test_breaking_scoped_feat_is_listed_and_raises_minor
run_test "a fix with a BREAKING CHANGE footer is listed and raises only the patch number" \
  test_breaking_change_footer_is_listed_and_raises_patch
run_test "a fix with a BREAKING-CHANGE footer is listed and raises only the patch number" \
  test_breaking_change_hyphen_footer_is_listed
run_test "a beta run lists a breaking feat in the run summary and releases 0.4.0-beta.1" \
  test_breaking_feat_beta_is_listed
run_test "a breaking commit released before stable 1.0.0 is not listed again" \
  test_breaking_commit_before_last_stable_is_not_listed
run_test "a commit that is not breaking is not listed in the run summary" \
  test_non_breaking_commit_is_not_listed
run_test "a breaking commit is listed on stderr and the version printed when no run summary file is set" \
  test_breaking_commit_listed_without_summary_file
run_test "the breaking list is appended to the run summary, keeping what earlier steps wrote" \
  test_summary_is_appended_not_replaced
run_test "stable 0.11.0 is the last stable release over 0.10.0 and 0.9.0 tagged on later commits" \
  test_highest_stable_by_version_order
run_test "0.4.0-beta.10 is the highest beta over 0.4.0-beta.9, so the next is 0.4.0-beta.11" \
  test_highest_beta_by_version_order
run_test "a 0.5.0 beta does not change the beta number of 0.4.0" \
  test_betas_of_another_version_are_ignored
run_test "a v-prefixed tag is not taken as the last stable release" \
  test_v_prefixed_tag_is_not_a_stable_release
run_test "the script works when run from a subdirectory of the repository" \
  test_runs_from_a_subdirectory
run_test "an unknown release kind is bad usage and exits 2" \
  test_unknown_kind_is_bad_usage
run_test "major ticked on a beta run is bad usage and exits 2" \
  test_major_with_beta_is_bad_usage

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures of $test_count tests failed"
  exit 1
fi
echo "all $test_count tests passed"
