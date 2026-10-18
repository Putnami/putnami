#!/usr/bin/env bash
# Builds the repository the language parity test scans: two commits with fixed
# identities and dates, so every commit hash is the same on every machine.
#
# Usage: fixture.sh <empty-directory>
set -euo pipefail

dir="${1:?usage: fixture.sh <empty-directory>}"
mkdir -p "$dir"
cd "$dir"

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=Fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=Fixture GIT_COMMITTER_EMAIL=fixture@example.invalid

git init -q
git symbolic-ref HEAD refs/heads/main
git config commit.gpgsign false

mkdir -p docs fixtures
printf 'An English readme.\nIt explains the project.\n' > README.md
printf 'This page is written in English.\n' > docs/clean.md
printf 'Nous corrigeons le code avec le fichier.\nCe fichier est dans le dépôt.\n' > docs/francais.md
printf 'Nous avons ajouté une école.\n' > fixtures/sample.md
printf 'binary\0data\n' > payload.bin
git add -A
GIT_AUTHOR_DATE='2026-01-01T00:00:00Z' GIT_COMMITTER_DATE='2026-01-01T00:00:00Z' \
  git commit -q -m 'chore: base'

printf 'The page grew by one line.\n' >> docs/clean.md
printf 'Il faut que nous écrivions cette page, parce que le texte est utile.\n' > docs/nouveau.md
git add -A
GIT_AUTHOR_DATE='2026-01-02T00:00:00Z' GIT_COMMITTER_DATE='2026-01-02T00:00:00Z' \
  git commit -q -m 'fix: Nous corrigeons le code' -m 'Le fichier est dans le dépôt.'
