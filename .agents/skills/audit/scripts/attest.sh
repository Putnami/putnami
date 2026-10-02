#!/usr/bin/env bash
# Attestation ledger for /audit.
#
# Records "this (project, group) was scanned at tree hash X" so the next wave
# can skip projects whose content has not changed since the last scan. This is
# the audit analogue of the putnami task cache: judgments are cacheable outputs
# keyed by input content.
#
# State lives in .putnami/audit/attestations/ (Putnami CLI state, gitignored). One flat
# JSON file per (project-short, group) so parallel fleet shards never contend;
# slashes in the Putnami project short name are flattened to '-'.
#
# Usage:
#   attest.sh hash <project-path>                  # print content hash for a project dir (folds direct deps)
#   attest.sh rubric                                # print the rubric hash (SKILL.md + mechanical.sh + config.sh + repository profile and checks)
#   attest.sh repo                                  # print the derived owner/repo key component
#   attest.sh check <project-short> <group> <hash> # exit 0 if attested at this hash (skip scan)
#   attest.sh record <project-short> <group> <hash>  # record a completed scan
#   attest.sh seed <project-short> <group> <hash>    # seed locally after a batch prefetch
#   attest.sh list                                  # show all attestations
#   attest.sh clear [<project-short>]               # drop all, or one project's, attestations
#
# Remote tier: when the root manifest enables Intelligence, `check`
# consults the shared tier after a local miss (seeding local on a hit) and `record`
# best-effort mirrors to it, via `putnami cloud audit-attest`. It is a pure
# optimization: any failure degrades to local-only and never fails an audit.
#
# Hash semantics: a stable fold of `git rev-parse HEAD:<path>` (the project's own
# tree object at HEAD) with the tree objects of its DIRECT dependencies (resolved
# from `putnami projects describe`), so a change in a direct dependency
# invalidates the dependent's attestation. A dirty working tree in the project's
# path OR any direct dependency's path appends "-dirty" so the hash never matches
# and the project is re-scanned. If `putnami projects describe` is unavailable the
# dep set is unknown, so the hash is marked unstable (re-scan) rather than silently
# folding own-tree-only. Indirect (transitive) dep changes are not folded in; the
# weekly --no-cache hygiene wave bounds that blind spot. Use --no-cache on /audit
# for a full forced wave.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
DIR="$ROOT/.putnami/audit/attestations"
HERE="$(cd "$(dirname "$0")" && pwd)"

usage() {
  sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

# ---------------------------------------------------------------------------
# Remote Intelligence attestation tier — an OPTIONAL, fail-open layer. When
# the manifest enables Intelligence, `check` consults the shared Intelligence tier
# after a local miss and `record` best-effort mirrors to it, via
# `putnami cloud audit-attest get|set`. The shared key is
# (repo, project, group, inputHash, rubricHash); repo comes from the git remote
# (override with PUTNAMI_AUDIT_ATTEST_REPO), rubricHash from compute_rubric. Every
# Intelligence call runs in a `set +e` subshell so a missing CLI, an
# unconfigured or unreachable service, or any error degrades to local-only and
# NEVER fails an audit.
# ---------------------------------------------------------------------------

remote_enabled() {
  bash "$HERE/config.sh" shared-attestations
}

# intelligence_timeout runs its args under a short timeout when `timeout` is
# available, so an unreachable service that HANGS (blackholed/half-open TCP)
# degrades instead of stalling the whole wave. Where `timeout` is absent (e.g.
# stock macOS) it runs the command directly. Override with
# PUTNAMI_AUDIT_ATTEST_TIMEOUT (seconds).
intelligence_timeout() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "${PUTNAMI_AUDIT_ATTEST_TIMEOUT:-8}" "$@"
  else
    "$@"
  fi
}

# attest_repo prints the stable "owner/repo" key component: PUTNAMI_AUDIT_ATTEST_REPO
# if set, else the last two path segments of the git remote (host/port ignored so
# the value is identical across machines and proxies).
attest_repo() {
  if [ -n "${PUTNAMI_AUDIT_ATTEST_REPO:-}" ]; then
    printf '%s' "$PUTNAMI_AUDIT_ATTEST_REPO"
    return 0
  fi
  local url
  url="$(git -C "$ROOT" config --get remote.origin.url 2>/dev/null)" || return 1
  [ -n "$url" ] || return 1
  url="${url%.git}"; url="${url%/}"; url="${url//:/\/}"
  local repo owner
  repo="${url##*/}"; url="${url%/*}"; owner="${url##*/}"
  [ -n "$owner" ] && [ -n "$repo" ] || return 1
  printf '%s/%s' "$owner" "$repo"
}

# compute_rubric folds the rubric inputs' git blob hashes (see the `rubric`
# command) into one stable hash; shared by the command and the remote key.
compute_rubric() {
  local bundle inputs f path
  bundle="$(cd "$HERE/.." && pwd)"
  inputs=""
  for f in SKILL.md scripts/mechanical.sh scripts/config.sh; do
    path="$bundle/$f"
    if [ -f "$path" ]; then
      inputs="${inputs}$(git hash-object "$path" 2>/dev/null || echo unreadable) $f"$'\n'
    else
      inputs="${inputs}missing $f"$'\n'
    fi
  done
  # The repository profile and checks script extend the rubric; a moved or
  # edited file is a new rubric. A configured file that is missing, or a
  # missing jq, makes config.sh exit 2, which folds in as "unreadable" rather
  # than vanishing.
  local rel status kind
  for kind in profile checks; do
    status=0
    rel="$(bash "$HERE/config.sh" "$kind" 2>/dev/null)" || status=$?
    case "$status" in
      0) inputs="${inputs}$(git hash-object "$ROOT/$rel" 2>/dev/null || echo unreadable) $rel"$'\n' ;;
      1) ;;
      *) inputs="${inputs}unreadable $kind"$'\n' ;;
    esac
  done
  printf '%s' "$inputs" | git hash-object --stdin
}

# audit_version prints the stamp recorded with each attestation. The workflow
# ships as @putnami/intelligence agent content, whose version is the
# extension's; the rubric hash already names the exact instructions.
audit_version() {
  printf 'rubric-%.12s' "$(compute_rubric)"
}

# record_local writes/overwrites the local ledger entry for (short, group, hash).
record_local() {
  local short="$1" group="$2" hash="$3"
  mkdir -p "$DIR"
  jq -n \
    --arg project "$short" --arg group "$group" --arg treeHash "$hash" \
    --arg headSha "$(git -C "$ROOT" rev-parse HEAD 2>/dev/null)" \
    --arg verifiedAt "$(date +%Y-%m-%d)" \
    '{project: $project, group: $group, treeHash: $treeHash, headSha: $headSha, verifiedAt: $verifiedAt}' \
    > "$DIR/${short//\//-}__${group}.json"
}

# remote_get exits 0 iff Intelligence has an attestation for this key. Runs in a
# `set +e` subshell: any failure (no CLI, unconfigured/unreachable service, or
# non-JSON output) is a MISS, never an abort.
remote_get() ( set +e
  short="$1"; group="$2"; hash="$3"
  repo="$(attest_repo)"; [ -n "$repo" ] || exit 1
  rh="$(compute_rubric)"; [ -n "$rh" ] || exit 1
  out="$(intelligence_timeout putnami cloud audit-attest get --json \
    --repo "$repo" --project "$short" --group "$group" \
    --input-hash "$hash" --rubric-hash "$rh" 2>/dev/null)" || exit 1
  # `--json` wraps the payload in the standard {command,status,data,exitCode}
  # envelope, so the hit flag is at .data.found; fall back to a top-level .found
  # for a bare shape.
  [ "$(printf '%s' "$out" | jq -r '.data.found // .found // false' 2>/dev/null)" = "true" ]
)

# remote_set best-effort mirrors an attestation to Intelligence. Runs in a
# `set +e` subshell and always exits 0 — mirroring must never fail an audit.
remote_set() ( set +e
  short="$1"; group="$2"; hash="$3"
  repo="$(attest_repo)"; [ -n "$repo" ] || exit 0
  rh="$(compute_rubric)"; [ -n "$rh" ] || exit 0
  intelligence_timeout putnami cloud audit-attest set --json \
    --repo "$repo" --project "$short" --group "$group" \
    --input-hash "$hash" --rubric-hash "$rh" \
    --head-sha "$(git -C "$ROOT" rev-parse HEAD 2>/dev/null)" \
    --audit-version "$(audit_version)" >/dev/null 2>&1
  exit 0
)

cmd="${1:-}"
[ -n "$cmd" ] || usage
shift

case "$cmd" in
  hash)
    path="${1:?usage: attest.sh hash <project-path>}"
    rel="${path#/}"
    rel="${rel%/}"
    # A path not in HEAD can never attest. git rev-parse echoes the arg back on a
    # bad rev, so use --verify -q (prints the object id, or nothing + non-zero) to
    # detect a missing tree cleanly.
    if ! own="$(git -C "$ROOT" rev-parse --verify -q "HEAD:$rel" 2>/dev/null)"; then
      echo "no-tree"
      exit 0
    fi
    dirty=""
    if [ -n "$(git -C "$ROOT" status --porcelain -- "$rel")" ]; then
      dirty="-dirty"
    fi
    # Fold the git tree objects of the project's DIRECT dependencies into the
    # hash so a dependency change invalidates this project's attestation. Dep ids
    # from `putnami projects describe` are "/<path>"; they are sorted for an
    # order-independent, deterministic fold, and a dirty dep tree propagates the
    # -dirty marker so it never attests.
    #
    # If `putnami projects describe` is UNAVAILABLE (empty output — a successful
    # describe always prints the project object), the dep set is unknown, so mark
    # the hash unstable (-dirty) rather than silently folding own-tree-only. A
    # silent own-tree-only fallback would let a dependency change go undetected
    # whenever describe is unreachable during both record and check, skipping a
    # stale project — the exact blind spot the fold exists to close. Marking it
    # unstable re-scans instead (never attesting on a partial hash) and still
    # never blocks the audit.
    dep_lines=""
    describe="$(putnami projects describe "/$rel" --output=jsonl 2>/dev/null || true)"
    if [ -z "$describe" ]; then
      dirty="-dirty"
    else
      while IFS= read -r depid; do
        [ -n "$depid" ] || continue
        deprel="${depid#/}"
        dtree="$(git -C "$ROOT" rev-parse --verify -q "HEAD:$deprel" 2>/dev/null || echo "no-tree")"
        if [ -n "$(git -C "$ROOT" status --porcelain -- "$deprel")" ]; then
          dirty="-dirty"
        fi
        dep_lines="${dep_lines}${deprel}=${dtree}"$'\n'
      done < <(printf '%s\n' "$describe" | jq -r '.dependencies[]?' 2>/dev/null | sort)
    fi
    folded="$(printf 'self=%s\n%s' "$own" "$dep_lines" | git hash-object --stdin)"
    echo "${folded}${dirty}"
    ;;
  rubric)
    # Rubric hash: a stable fold of the audit rubric inputs (SKILL.md +
    # mechanical.sh + config.sh, which derives the module list, + the
    # repository profile and checks) so a rubric or profile edit
    # invalidates attestations implicitly (no manual --no-cache). Independent of
    # any project; the shared audit key combines it with the per-project input
    # hash. See compute_rubric.
    compute_rubric
    ;;
  repo)
    # Print the derived "owner/repo" key component (see attest_repo); fleet.sh uses
    # it to build the prefetch batch with the same repo the remote tier keys on.
    r="$(attest_repo)" || { echo "attest: no git remote to derive repo" >&2; exit 1; }
    printf '%s\n' "$r"
    ;;
  check)
    short="${1:?project-short}"; group="${2:?group}"; hash="${3:?hash}"
    # An unstable hash (dirty tree / missing path) never attests, local or cloud.
    case "$hash" in *-dirty|no-tree*) exit 1 ;; esac
    # Tier 1 — local ledger.
    f="$DIR/${short//\//-}__${group}.json"
    if [ -f "$f" ] && [ "$(jq -r '.treeHash' "$f" 2>/dev/null)" = "$hash" ]; then
      exit 0
    fi
    # Tier 2 — shared Intelligence tier (opt-in, fail-open). On a hit, seed the
    # local ledger so subsequent checks (this run and other shards on this
    # machine) resolve locally without another round trip.
    if remote_enabled && remote_get "$short" "$group" "$hash"; then
      record_local "$short" "$group" "$hash" || true
      exit 0
    fi
    exit 1
    ;;
  record)
    short="${1:?project-short}"; group="${2:?group}"; hash="${3:?hash}"
    case "$hash" in
      *-dirty|no-tree*)
        echo "attest: refusing to record unstable hash '$hash' (dirty tree or untracked path)" >&2
        exit 1
        ;;
    esac
    record_local "$short" "$group" "$hash"
    # Best-effort mirror to the shared Intelligence tier (opt-in, fail-open).
    if remote_enabled; then
      remote_set "$short" "$group" "$hash" || true
    fi
    ;;
  seed)
    # Fleet prefetch already performed the one shared lookup. Seed its hits in
    # the local first tier without issuing one redundant PUT per hit.
    short="${1:?project-short}"; group="${2:?group}"; hash="${3:?hash}"
    case "$hash" in *-dirty|no-tree*) exit 1 ;; esac
    record_local "$short" "$group" "$hash"
    ;;
  list)
    if [ ! -d "$DIR" ] || [ -z "$(ls -A "$DIR" 2>/dev/null)" ]; then
      echo "no attestations recorded"
      exit 0
    fi
    printf '%-24s %-24s %-14s %s\n' PROJECT GROUP DATE TREE
    for f in "$DIR"/*.json; do
      jq -r '[.project, .group, .verifiedAt, .treeHash[0:12]] | @tsv' "$f"
    done | sort | while IFS=$'\t' read -r p g d t; do
      printf '%-24s %-24s %-14s %s\n' "$p" "$g" "$d" "$t"
    done
    ;;
  clear)
    short="${1:-}"
    if [ -n "$short" ]; then
      rm -f "$DIR/${short//\//-}__"*.json
    else
      rm -f "$DIR"/*.json 2>/dev/null || true
    fi
    ;;
  *)
    usage
    ;;
esac
