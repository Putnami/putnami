#!/usr/bin/env bash
#
# orient-bench.sh — agent-context orientation-reads benchmark.
#
# What it measures
# ----------------
# "Manual orientation" is the number of filesystem round-trips an agent makes to
# reconstruct, by hand, the facts that one `putnami context pack` document already
# aggregates for a project. At minimum that is:
#
#   1 directory enumeration (discover what is in the project)
#   + K reads          (one read per referenced committed file: composition roots,
#                       capability/contract/infra/migration refs, representative
#                       sources, adjacent docs, config schema)
#   + 1 graph reconstruction (rebuild the dependency graph from the manifests)
#
# i.e. >= K + 2 round-trips per project. The read-only MCP `agent_context` tool
# returns all of it in exactly 1 structured call, after which the agent reads only
# the specific source RANGES it actually needs.
#
# What it does
# ------------
# For a fixed, deterministic list of Go + TypeScript samples it runs
# `putnami context pack --project <id>`, parses each emitted (gitignored,
# ephemeral) `<project>/.gen/agent-context.json` with python3 (NOT jq), and prints
# a table:
#
#   sample | referenced files K | graph deps D | composition roots | tests
#          | manual round-trips (>= K+2) | agent_context (1 call)
#
# Output order is fixed by the sample list below (no map iteration), so the table
# is reproducible. Structural counts (K, D, roots) are stable across commits;
# digests and the workspace revision embedded in the artifact are not, and are
# deliberately not printed here.
#
# Reproduce:  bash protocols/agentcontext/bench/orient-bench.sh
#
set -euo pipefail

# Repo root, relative to this script (works regardless of cwd).
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Fixed, ordered sample list: Go first, then TypeScript. Includes the sparse
# `capabilities-proof` case on purpose (see README.md).
SAMPLES="
/go/samples/unit-of-work-proof
/go/samples/task-api
/go/samples/capabilities-proof
/typescript/samples/06-database
/typescript/samples/14-capabilities
"

echo "Packing agent-context for ${SAMPLES//$'\n'/ }" >&2
for id in $SAMPLES; do
  ./putnamiw context pack --project "$id" >/dev/null
done

# Parse every emitted document and print the table in one deterministic pass.
# Sample ids are passed as argv so python does no globbing / map iteration.
python3 - "$REPO_ROOT" $SAMPLES <<'PY'
import json
import os
import sys

repo_root = sys.argv[1]
ids = sys.argv[2:]

REF_LIST_FIELDS = (
    "compositionRoots",
    "capabilities",
    "contracts",
    "infra",
    "migrations",
    "representativeSources",
    "docs",
)


def referenced_files(doc):
    """Distinct workspace-relative file paths the manifest points at."""
    paths = set()
    for field in REF_LIST_FIELDS:
        for entry in doc.get(field) or []:
            p = entry.get("path")
            if p:
                paths.add(p)
    cfg = doc.get("config") or {}
    schema_ref = cfg.get("schemaRef") or {}
    if schema_ref.get("path"):
        paths.add(schema_ref["path"])
    return len(paths)


def tests_summary(doc):
    tests = doc.get("tests")
    if not tests:
        return "no-section"
    packs = tests.get("packs") or []
    if packs:
        return "%d conformance pack%s" % (len(packs), "" if len(packs) == 1 else "s")
    return tests.get("absenceReason") or "no-packs"


rows = []
for pid in ids:
    rel = pid.lstrip("/")
    artifact = os.path.join(repo_root, rel, ".gen", "agent-context.json")
    with open(artifact) as fh:
        doc = json.load(fh)
    k = referenced_files(doc)
    deps = len(doc.get("identity", {}).get("dependencies", []) or [])
    roots = len(doc.get("compositionRoots") or [])
    rows.append(
        {
            "sample": rel,
            "k": k,
            "deps": deps,
            "roots": roots,
            "tests": tests_summary(doc),
            "manual": k + 2,
            "agent": 1,
        }
    )

headers = [
    ("sample", "sample"),
    ("k", "ref files K"),
    ("deps", "graph deps D"),
    ("roots", "comp roots"),
    ("tests", "tests"),
    ("manual", "manual round-trips (>=K+2)"),
    ("agent", "agent_context"),
]

widths = {}
for key, title in headers:
    widths[key] = len(title)
    for row in rows:
        widths[key] = max(widths[key], len(str(row[key])))


def fmt(values):
    return " | ".join(str(values[key]).ljust(widths[key]) for key, _ in headers)


print(fmt({key: title for key, title in headers}))
print("-+-".join("-" * widths[key] for key, _ in headers))
for row in rows:
    print(fmt(row))

print()
total_manual = sum(row["manual"] for row in rows)
total_agent = sum(row["agent"] for row in rows)
print(
    "Totals across %d projects: manual orientation >= %d filesystem round-trips vs "
    "%d `agent_context` calls." % (len(rows), total_manual, total_agent)
)
PY
