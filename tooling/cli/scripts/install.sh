#!/bin/bash
# Putnami CLI Install Script
# Usage: curl -fsSL https://putnami.dev/install.sh | bash
#        curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash
#
# This script installs the Putnami CLI for building, testing, linting, and deploying applications.
# With a command to run (?run=, --run, or PUTNAMI_RUN), it then pins the extension
# that provides the command for the current user and runs `putnami <command>` in
# the caller's directory, which it never changes or writes to.
#
# Install trust model — see doc/22-installing-the-cli.md and ADR 0012:
#   - the download is compared against the SHA-256 the registry advertises for
#     it, and the word "verified" is printed only after that comparison passes;
#   - a download nobody vouched for is refused, not installed
#     (PUTNAMI_UNSAFE_INSTALL=1 overrides, loudly);
#   - the installed binary's own --version must match the version the registry
#     resolved, so a stale artifact cannot be installed under a fresh tag;
#   - the registry must be reachable over https, or loopback http;
#   - the CLI is not downloaded before a writable install directory exists;
#   - the installer never escalates privileges. It writes only where the
#     invoking user can already write;
#   - a command to run is resolved from the command map before anything is
#     written. The map is unsigned and served next to this script, over the same
#     https-or-loopback rule, so it is trusted exactly as far as this script is.
#     The extension it names is installed by the CLI, which verifies that
#     extension's SHA-256 like any other extension install.
#
# Prerequisites: curl, tar, and a SHA-256 tool (sha256sum or shasum).

set -e

# Release settings
BINARY_NAME="putnami"
DEFAULT_REGISTRY_URL="https://put.putnami.dev"
INSTALL_SCRIPT_URL="https://putnami.dev/install.sh"
INSTALL_PS1_URL="https://putnami.dev/install.ps1"

# The command to run after installing. The site sets the value of this one line
# for https://putnami.dev/install.sh?run=<command> and changes nothing else in
# the script; --run and PUTNAMI_RUN take precedence over it.
RUN_COMMAND_DEFAULT=""

# The command map: which extension provides each command the installer can run.
# Its first line is COMMAND_MAP_HEADER; every other line is blank, a # comment,
# or "<command> <@scope/name[@constraint]>". Anything else refuses the run.
DEFAULT_COMMAND_MAP_URL="https://putnami.dev/install-commands.txt"
COMMAND_MAP_URL_ENV="PUTNAMI_COMMAND_MAP_URL"
COMMAND_MAP_HEADER="putnami.install-commands.v1"
RUN_ENV="PUTNAMI_RUN"

# Character sets are spelled out rather than written as ranges: a range such as
# a-z follows the locale's collation order in some regex implementations and can
# admit uppercase or non-ASCII letters.
LOWER_ALPHA="abcdefghijklmnopqrstuvwxyz"
UPPER_ALPHA="ABCDEFGHIJKLMNOPQRSTUVWXYZ"
DIGITS="0123456789"
BLANKS=" "$'\t'
# A command the installer can run: ^[a-z][a-z0-9-]{0,63}$. The site holds
# ?run= to the same rule before it sets RUN_COMMAND_DEFAULT.
RUN_COMMAND_PATTERN="^[${LOWER_ALPHA}][${LOWER_ALPHA}${DIGITS}-]{0,63}\$"
# An extension in the command map: @scope/name, optionally followed by
# @<constraint>, an optional ^ or ~ and then letters, digits, and . + _ -.
EXTENSION_REF_PATTERN="^@[${LOWER_ALPHA}${DIGITS}][${LOWER_ALPHA}${DIGITS}._-]{0,127}/[${LOWER_ALPHA}${DIGITS}][${LOWER_ALPHA}${DIGITS}._-]{0,127}(@[~^]?[${LOWER_ALPHA}${UPPER_ALPHA}${DIGITS}][${LOWER_ALPHA}${UPPER_ALPHA}${DIGITS}.+_-]{0,63})?\$"
# One command map entry: two fields separated by blanks, nothing before or after.
COMMAND_MAP_ENTRY_PATTERN="^([^${BLANKS}]+)[${BLANKS}]+([^${BLANKS}]+)\$"
# The largest command map the installer reads, in bytes.
COMMAND_MAP_MAX_BYTES=1048576

# Host and binary targets this script installs: the darwin|linux x amd64|arm64
# part of the repository's First Public-Release Contract. Its windows/amd64
# target installs through install.ps1. Anything else is refused, not warned
# about — a warning that is followed by an install produces a broken binary at
# the moment the user first runs it, with no evidence of why.
SUPPORTED_MATRIX="darwin (macOS) and linux, on amd64 or arm64"

# Opt-out for the fail-closed integrity check. Mirrors PUTNAMI_UNSAFE_UPDATE on
# the in-binary `putnami upgrade` path; routine use removes the only defense
# against a tampered download.
UNSAFE_INSTALL_ENV="PUTNAMI_UNSAFE_INSTALL"
ALLOW_INSECURE_REGISTRY_ENV="PUTNAMI_ALLOW_INSECURE_REGISTRY"
EXPECTED_SHA256_ENV="PUTNAMI_EXPECTED_SHA256"

# Opt-out for agent-host registration. Installing the CLI otherwise writes a
# user-scoped MCP entry into two other applications, which an image build, a CI
# cache warm, or a user who configures those hosts by hand has every reason to
# decline. Declining changes nothing else about the install.
NO_AGENT_HOSTS_ENV="PUTNAMI_NO_AGENT_HOSTS"
NO_AGENT_HOSTS="${PUTNAMI_NO_AGENT_HOSTS:-}"

# Scratch directory for the download, removed by the EXIT trap. It is global on
# purpose: the trap body expands $TEMP_DIR when the shell exits, which on a
# successful run is *after* main has returned. A `local` here is out of scope by
# then, so the trap would expand to "" and silently rm -rf nothing, leaking the
# downloaded binary and response headers on every successful install. Failure
# paths cleaned up correctly only because `exit` fires while main is still on
# the stack.
TEMP_DIR=""

# The line that makes the command reachable in the shell that ran the
# installer, or "" when that shell already reaches it. The footer prints it
# first: a script running as the child of a pipe cannot change its parent's PATH.
PATH_LINE_TO_RUN=""

# Colors, off unless the stream the installer's messages go to (file descriptor
# $1) is a terminal, or when NO_COLOR is set. A piped or redirected run is what
# CI, the release smoke, and the installer's own tests read; an escape sequence
# in the middle of a line is what makes a grep for "Integrity verified" fail on a
# script that verified the download correctly.
configure_colors() {
    local fd="$1"
    if [[ -t "$fd" && -z "${NO_COLOR:-}" ]]; then
        RED='\033[0;31m'
        GREEN='\033[0;32m'
        YELLOW='\033[1;33m'
        CYAN='\033[0;36m'
        BOLD='\033[1m'
        DIM='\033[2m'
        NC='\033[0m'
    else
        RED=''
        GREEN=''
        YELLOW=''
        CYAN=''
        BOLD=''
        DIM=''
        NC=''
    fi

    CHECKMARK="${GREEN}✓${NC}"
    CROSS="${RED}✗${NC}"
    ARROW="${CYAN}→${NC}"
    SPARKLE="${YELLOW}✨${NC}"
}

configure_colors 1

# Print functions.
#
# Diagnostics go to stderr: several helpers return their result on stdout
# through a command substitution, so a diagnostic on stdout would be captured
# as data and never reach the user.
print_step() {
    echo -e "${ARROW} ${BOLD}$1${NC}"
}

print_detail() {
    echo -e "  ${DIM}$1${NC}"
}

print_success() {
    echo -e "${CHECKMARK} $1"
}

print_error() {
    echo -e "${CROSS} ${RED}$1${NC}" >&2
}

print_hint() {
    echo -e "  ${DIM}$1${NC}" >&2
}

print_warning() {
    echo -e "${YELLOW}⚠${NC}  $1" >&2
}

print_info() {
    echo -e "${DIM}$1${NC}"
}

trim() {
    local value="$1"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "$value"
}

to_lower() {
    printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

# Check if required commands are available.
#
# A SHA-256 tool is as required as curl: without one the download cannot be
# compared to the advertised digest, and an install that cannot be verified is
# not performed. The one documented escape hatch is the same one that covers a
# registry advertising no digest at all.
check_prerequisites() {
    if ! command -v curl >/dev/null 2>&1; then
        print_error "curl is required to download Putnami CLI"
        exit 1
    fi
    if ! command -v tar >/dev/null 2>&1; then
        print_error "tar is required to extract Putnami CLI archives"
        exit 1
    fi
    if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
        if [[ "${PUTNAMI_UNSAFE_INSTALL:-}" == "1" ]]; then
            return 0
        fi
        print_error "sha256sum or shasum is required to verify the download; refusing to install an unverified binary."
        print_hint "Install GNU coreutils (sha256sum) or perl's shasum, then re-run."
        print_hint "Set ${UNSAFE_INSTALL_ENV}=1 to install without verification."
        exit 1
    fi
}

# Detect OS
detect_os() {
    case "$(uname -s)" in
        Darwin*)    echo "darwin" ;;
        Linux*)     echo "linux" ;;
        CYGWIN*|MINGW*|MSYS*) echo "windows" ;;
        *)          echo "unknown" ;;
    esac
}

# Detect architecture.
#
# The vocabulary is Go's GOARCH (amd64/arm64), which is what `putnami upgrade`
# and the release smoke already send to the same download endpoint. One client
# speaking a private dialect is how a platform silently stops being covered.
# The registry stores darwin-x64/linux-x64 and maps amd64 onto x64 itself.
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        *) echo "unknown" ;;
    esac
}

# Refuse any platform outside the supported matrix before anything is fetched.
require_supported_platform() {
    local os="$1"
    local arch="$2"

    # A Windows shell (Git Bash, MSYS2, Cygwin) cannot install the native
    # Windows CLI from here; the PowerShell installer does. Microsoft Defender
    # blocks the one-liner passed to powershell on a command line, so a shell
    # other than PowerShell downloads the script and runs it with -File.
    if [[ "$os" == "windows" ]]; then
        print_error "install.sh does not install on Windows. Use the PowerShell installer:"
        print_hint "In a PowerShell window: irm ${INSTALL_PS1_URL} | iex"
        print_hint "From cmd.exe or this shell: curl.exe -fsSLo install.ps1 ${INSTALL_PS1_URL} && powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1"
        exit 1
    fi
    if [[ "$os" == "unknown" ]]; then
        print_error "Unsupported operating system: $(uname -s)"
        print_hint "Supported: ${SUPPORTED_MATRIX}."
        exit 1
    fi
    if [[ "$arch" == "unknown" ]]; then
        print_error "Unsupported architecture: $(uname -m)"
        print_hint "Supported: ${SUPPORTED_MATRIX}."
        exit 1
    fi
}

# Replace any user:token@ in a URL with ***@ before it reaches stdout.
# PUTNAMI_REGISTRY_URL is allowed to carry credentials, and the release smoke
# tails this script's log straight into CI output, so a URL printed verbatim
# publishes the token. Userinfo precedes the first '@' of the authority, which
# ends at the first '/' after the scheme.
redact_url() {
    local url="$1"
    local scheme="" rest="$url"

    if [[ "$url" == *"://"* ]]; then
        scheme="${url%%://*}://"
        rest="${url#*://}"
    fi
    if [[ "${rest%%/*}" == *@* ]]; then
        rest="***@${rest#*@}"
    fi
    printf '%s%s' "$scheme" "$rest"
}

# Reject a URL that cannot carry an authenticated download. Mirrors
# extension.ValidateRegistryURL: https is always fine, http only for loopback
# hosts or with an explicit opt-in, and no other scheme at all.
validate_registry_url() {
    local raw="$1"
    local origin="$2"

    if [[ "$raw" != *"://"* ]]; then
        print_error "Invalid ${origin} \"$(redact_url "$raw")\": no scheme (expected https://...)"
        return 1
    fi

    local scheme
    scheme="$(to_lower "${raw%%://*}")"

    local rest="${raw#*://}"
    local authority="${rest%%/*}"
    authority="${authority##*@}"

    local host
    if [[ "$authority" == \[*\]* ]]; then
        host="${authority%%\]*}]"
    else
        host="${authority%%:*}"
    fi
    host="$(to_lower "$host")"

    case "$scheme" in
        https)
            return 0
            ;;
        http)
            case "$host" in
                localhost|127.0.0.1|::1|"[::1]") return 0 ;;
            esac
            if [[ "${PUTNAMI_ALLOW_INSECURE_REGISTRY:-}" == "1" ]]; then
                return 0
            fi
            print_error "${origin} \"$(redact_url "$raw")\" uses plaintext http://; refusing to fetch over an unauthenticated channel."
            print_hint "Switch to https://, or set ${ALLOW_INSECURE_REGISTRY_ENV}=1 to override."
            return 1
            ;;
        *)
            print_error "${origin} \"$(redact_url "$raw")\" has unsupported scheme \"${scheme}\" (only https and http are accepted)."
            return 1
            ;;
    esac
}

# Read the last occurrence of a response header. curl -D appends one block per
# response, so after a redirect the last block is the one that carried the body.
read_header() {
    local headers_file="$1"
    local name="$2"

    [[ -f "$headers_file" ]] || return 0
    grep -i "^${name}:" "$headers_file" 2>/dev/null \
        | tail -n 1 \
        | sed -e 's/^[^:]*:[[:space:]]*//' \
        | tr -d '\r'
}

# Return the SHA-256 the registry advertised for the bytes it just streamed, or
# "" when it advertised none. Mirrors extension.ReadAdvertisedIntegrity:
# X-Integrity wins, otherwise the sha-256 member of an RFC 9530 Digest header.
read_advertised_integrity() {
    local headers_file="$1"

    local value
    value="$(trim "$(read_header "$headers_file" "X-Integrity")")"
    if [[ -n "$value" ]]; then
        printf '%s' "$value"
        return 0
    fi

    local digest
    digest="$(read_header "$headers_file" "Digest")"
    if [[ -z "$digest" ]]; then
        return 0
    fi

    local IFS=','
    local part
    for part in $digest; do
        part="$(trim "$part")"
        if [[ "$(to_lower "$part")" == sha-256=* ]]; then
            trim "${part#*=}"
            return 0
        fi
    done
    return 0
}

# Convert an advertised integrity string into a bare lowercase hex SHA-256.
# Mirrors extension.NormalizeIntegrity exactly: it accepts sha256:/sha-256:/
# sha256-/sha-256- prefixes and raw hex, and rejects everything else so the
# comparison has one canonical path.
normalize_integrity() {
    local raw="$1"
    local origin="$2"

    local value
    value="$(trim "$raw")"
    local lowered
    lowered="$(to_lower "$value")"

    local prefix
    for prefix in "sha256:" "sha-256:" "sha256-" "sha-256-"; do
        if [[ "$lowered" == "${prefix}"* ]]; then
            value="${value:${#prefix}}"
            break
        fi
    done

    value="$(to_lower "$(trim "$value")")"
    if [[ ${#value} -ne 64 ]]; then
        print_error "Invalid integrity from ${origin} (\"${raw}\"): expected 64-character hex SHA-256, got ${#value} characters."
        return 1
    fi
    if [[ ! "$value" =~ ^[0-9a-f]{64}$ ]]; then
        print_error "Invalid integrity from ${origin} (\"${raw}\"): not valid hex."
        return 1
    fi

    printf '%s' "$value"
}

compute_sha256() {
    local file="$1"

    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
        return 0
    fi
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
        return 0
    fi
    return 1
}

# Fail closed on a download whose authenticity nobody vouched for.
#
# This is the whole point of the script's trust model: the same rule the
# in-binary `putnami upgrade` applies (VersionUpdateWithOptions), applied to the
# public curl-pipe path so the two cannot drift.
verify_download_integrity() {
    local asset_file="$1"
    local advertised="$2"
    local expected_override="$3"
    local source_label="$4"

    local expected=""
    if [[ -n "$expected_override" ]]; then
        expected="$(normalize_integrity "$expected_override" "--sha256")" || return 1
    fi

    local advertised_norm=""
    if [[ -n "$advertised" ]]; then
        advertised_norm="$(normalize_integrity "$advertised" "$source_label")" || return 1
    fi

    if [[ -n "$expected" && -n "$advertised_norm" && "$expected" != "$advertised_norm" ]]; then
        print_error "The digest you supplied does not match the one ${source_label} advertised; refusing to install."
        print_hint "--sha256:   ${expected}"
        print_hint "advertised: ${advertised_norm}"
        return 1
    fi
    if [[ -z "$expected" ]]; then
        expected="$advertised_norm"
    fi

    if [[ -z "$expected" ]]; then
        if [[ "${PUTNAMI_UNSAFE_INSTALL:-}" != "1" ]]; then
            print_error "${source_label} did not advertise an integrity hash; refusing to install an unverified binary."
            print_hint "Set ${UNSAFE_INSTALL_ENV}=1 to override."
            print_hint "With --download-url, pass --sha256 <hex> or set ${EXPECTED_SHA256_ENV}."
            return 1
        fi
        print_warning "Installing without integrity verification (${UNSAFE_INSTALL_ENV}=1)."
        return 0
    fi

    local actual=""
    if ! actual="$(compute_sha256 "$asset_file")"; then
        if [[ "${PUTNAMI_UNSAFE_INSTALL:-}" == "1" ]]; then
            print_warning "Installing without integrity verification (${UNSAFE_INSTALL_ENV}=1): no SHA-256 tool available."
            return 0
        fi
        print_error "Could not compute a SHA-256 for the download; refusing to install an unverified binary."
        print_hint "Install sha256sum or shasum, then re-run."
        return 1
    fi

    if [[ "$actual" != "$expected" ]]; then
        print_error "Integrity check failed: expected ${expected}, got ${actual}"
        print_hint "The download does not match the digest ${source_label} advertised. Nothing was installed."
        return 1
    fi

    print_success "Integrity verified ${DIM}(sha256:${expected})${NC}"
    return 0
}

# Refuse a binary whose embedded --version disagrees with the version the
# registry resolved. Mirrors verifyDownloadedBinaryStamp: the publisher refuses
# to produce such a binary, but a registry can still resolve a tag to a
# prior commit's artifact when release archives were not uploaded.
#
# Best effort in one direction only: a binary that reports no version at all is
# not blocked, so older archives and future --version formats still install.
verify_binary_stamp() {
    local binary_path="$1"
    local resolved_version="$2"

    if [[ -z "$resolved_version" ]]; then
        return 0
    fi

    local reported_line
    reported_line="$(PUTNAMI_NO_RELAUNCH=1 PUTNAMI_LAUNCHED='' "$binary_path" --version 2>/dev/null | head -n1 || true)"
    reported_line="$(trim "$reported_line")"
    if [[ -z "$reported_line" ]]; then
        return 0
    fi

    local reported
    reported="$(printf '%s' "$reported_line" | awk '{print $NF}')"
    if [[ -z "$reported" || "$(to_lower "$reported")" == "unknown" ]]; then
        return 0
    fi
    if [[ "${reported#v}" == "${resolved_version#v}" ]]; then
        print_success "Version stamp verified ${DIM}(${reported})${NC}"
        return 0
    fi

    print_error "The registry resolved ${resolved_version} but the downloaded binary reports \"${reported}\"; refusing to install a stale build."
    print_hint "The release archives for ${resolved_version} may not have been uploaded. Nothing was installed."
    return 1
}

# Normalize version selector:
# - semver inputs become v-prefixed (1.2.3 -> v1.2.3)
# - channel/tag inputs are kept as-is (canary, dev, latest)
normalize_tag() {
    local version="$1"
    if [[ "$version" == "latest" ]]; then
        echo "latest"
        return 0
    fi

    # Semver (optionally already prefixed with v)
    if [[ "$version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9._-]+)?$ ]]; then
        if [[ "$version" == v* ]]; then
            echo "$version"
        else
            echo "v${version}"
        fi
        return 0
    fi

    # Non-semver channels/tags (e.g., canary, dev)
    echo "$version"
}

# Probe an actual write rather than trusting the mode bits: a read-only mount,
# an ACL, or a full filesystem all pass `test -w` and then fail the install
# halfway through.
dir_is_writable() {
    local dir="$1"
    local probe="${dir}/.putnami-write-probe.$$"

    if (umask 077; : > "$probe") 2>/dev/null; then
        rm -f "$probe"
        return 0
    fi
    return 1
}

print_install_dir_remedy() {
    print_hint "Pick a writable location with --install-dir <dir>, or set PUTNAMI_INSTALL_DIR=<dir>."
    print_hint "This installer never escalates privileges — run it as a user who can write to the target."
}

# Resolve the install location before anything is downloaded, so a machine with
# no writable target fails in one second with a remedy instead of after a
# multi-megabyte download.
ensure_writable_install_dir() {
    local dir="$1"

    if [[ -e "$dir" && ! -d "$dir" ]]; then
        print_error "Install directory ${dir} exists and is not a directory."
        print_install_dir_remedy
        exit 1
    fi
    if [[ ! -d "$dir" ]]; then
        if ! mkdir -p "$dir" 2>/dev/null; then
            print_error "Cannot create install directory ${dir} (a parent is missing or not writable)."
            print_install_dir_remedy
            exit 1
        fi
    fi
    if ! dir_is_writable "$dir"; then
        print_error "Install directory ${dir} is not writable by $(id -un 2>/dev/null || echo "${USER:-this user}")."
        print_install_dir_remedy
        exit 1
    fi
}

# Put a file at its final path through a staged rename(2) — never rewrite the
# destination through its existing inode. On macOS, overwriting an executable
# while any process still runs the old image invalidates the kernel's cached
# code signature for that file; every exec of the path afterwards is killed or
# hangs uninterruptibly until reboot.
place_executable() {
    local source_binary="$1"
    local target_path="$2"

    local staging_path="${target_path}.new.$$"
    cp "$source_binary" "$staging_path"
    chmod +x "$staging_path"
    mv -f "$staging_path" "$target_path"
}

# Point a symlink at a new target without ever unlinking the live path: create
# the replacement beside it, then rename(2) over it, so a concurrent exec sees
# either the old target or the new one and never a missing file.
place_symlink() {
    local target="$1"
    local link_path="$2"

    local staging_link="${link_path}.new.$$"
    rm -f "$staging_link"
    ln -s "$target" "$staging_link" || return 1
    mv -f "$staging_link" "$link_path" || {
        rm -f "$staging_link"
        return 1
    }
}

# Install binary with a versioned name and point the `putnami` symlink at it.
# This is the layout `putnami version use` and `putnami upgrade --global`
# manage, so an install and an upgrade land in the same shape.
install_versioned_binary() {
    local source_binary="$1"
    local bin_dir="$2"
    local variant="$3"
    local version_tag="$4"

    mkdir -p "$bin_dir"

    local target_name="putnami-${variant}-${version_tag}"
    place_executable "$source_binary" "${bin_dir}/${target_name}"
    place_symlink "$target_name" "${bin_dir}/putnami"

    echo "${bin_dir}/${target_name}"
}

# Install binary into an explicitly requested directory, under the plain name.
install_binary() {
    local source_binary="$1"
    local install_dir="$2"

    mkdir -p "$install_dir"
    place_executable "$source_binary" "${install_dir}/${BINARY_NAME}"

    echo "${install_dir}/${BINARY_NAME}"
}

# Extract the downloaded asset and echo the path to the executable.
extract_binary() {
    local asset_file="$1"
    local temp_dir="$2"

    local bin_path=""

    # Sniff format by magic bytes — the registry serves a raw binary for
    # putnami/cli, but older releases still ship a .tar.gz, and the response
    # carries no reliable file name.
    local magic
    magic=$(od -An -N2 -tx1 "$asset_file" 2>/dev/null | tr -d ' \n')

    if [[ "$magic" == "1f8b" ]]; then
        tar -xzf "$asset_file" -C "$temp_dir"
        bin_path="$(find "$temp_dir" -type f -name "$BINARY_NAME" | head -n1)"
    elif [[ "$magic" == "504b" ]]; then
        if ! command -v unzip >/dev/null 2>&1; then
            print_error "unzip is required to extract this release archive"
            exit 1
        fi
        unzip -q "$asset_file" -d "$temp_dir"
        bin_path="$(find "$temp_dir" -type f -name "$BINARY_NAME" | head -n1)"
    else
        bin_path="$asset_file"
    fi

    if [[ -z "$bin_path" || ! -f "$bin_path" ]]; then
        print_error "Downloaded asset did not contain a '${BINARY_NAME}' executable"
        exit 1
    fi

    echo "$bin_path"
}

path_contains() {
    case ":${PATH}:" in
        *":$1:"*) return 0 ;;
        *) return 1 ;;
    esac
}

# Append a block to a shell startup file exactly once, keyed on a marker the
# block itself contains. The installer only ever appends: rewriting somebody's
# dotfile in place makes a failed install cost them their configuration.
#
# Exit status: 0 appended, 2 already present, 1 could not write.
append_once() {
    local file="$1"
    local marker="$2"
    local body="$3"

    mkdir -p "$(dirname "$file")" 2>/dev/null || true
    if [[ -f "$file" ]] && grep -qF -- "$marker" "$file" 2>/dev/null; then
        return 2
    fi
    if { printf '\n# Added by the Putnami CLI installer\n%s\n' "$body" >> "$file"; } 2>/dev/null; then
        return 0
    fi
    return 1
}

# Write the PATH line the user needs into the file their shell reads, and print
# the line to run in the shell they are in right now.
configure_shell_path() {
    local install_dir="$1"

    local shell_name
    shell_name="$(basename "${SHELL:-/bin/sh}")"

    local profile=""
    local path_line=""
    case "$shell_name" in
        zsh)
            profile="${ZDOTDIR:-$HOME}/.zshrc"
            path_line="export PATH=\"${install_dir}:\$PATH\""
            ;;
        bash)
            if [[ -f "${HOME}/.bash_profile" ]]; then
                profile="${HOME}/.bash_profile"
            else
                profile="${HOME}/.bashrc"
            fi
            path_line="export PATH=\"${install_dir}:\$PATH\""
            ;;
        fish)
            profile="${HOME}/.config/fish/config.fish"
            path_line="fish_add_path ${install_dir}"
            ;;
        *)
            profile="${HOME}/.profile"
            path_line="export PATH=\"${install_dir}:\$PATH\""
            ;;
    esac

    echo ""
    print_warning "${BINARY_NAME} is not on your PATH yet. Run this now, in this shell:"
    printf '  %b%s%b\n' "$CYAN" "$path_line" "$NC"
    PATH_LINE_TO_RUN="$path_line"

    local rc=0
    append_once "$profile" "$path_line" "$path_line" || rc=$?
    case "$rc" in
        0) print_success "Added it to ${profile} for future shells" ;;
        2) print_info "  ${profile} already sets it for future shells." ;;
        *) print_warning "Could not update ${profile}; add the line above by hand." ;;
    esac
}

# Symlink the installed binary into a user-writable directory that is already on
# PATH. No privilege escalation and no candidate outside the user's control: a
# curl-pipe installer that prompts for a password is one a user cannot audit
# before it runs.
link_into_path_dir() {
    local installed_binary="$1"

    local link_dir
    for link_dir in "${HOME}/.local/bin" "${HOME}/bin" "/usr/local/bin"; do
        path_contains "$link_dir" || continue
        # A neutral HOME commonly has ~/.local/bin on PATH before the directory
        # exists. Create only the two user-owned candidates; this is what makes
        # the curl-pipe install immediately discoverable in that same shell
        # without asking the user to pre-create state or restart it. Never
        # create a system directory here.
        if [[ ! -d "$link_dir" ]]; then
            case "$link_dir" in
                "${HOME}/.local/bin" | "${HOME}/bin")
                    mkdir -p "$link_dir" 2>/dev/null || continue
                    ;;
                *) continue ;;
            esac
        fi
        dir_is_writable "$link_dir" || continue

        local link_path="${link_dir}/${BINARY_NAME}"
        if place_symlink "$installed_binary" "$link_path" 2>/dev/null; then
            print_success "Linked ${link_path} → ${installed_binary}"
            return 0
        fi
    done
    return 1
}

# Say which binary `putnami` runs right now, and never claim more than that. The
# comparison is by inode (-ef follows symlinks), so an install reached through
# the versioned symlink still counts as reached, while an older copy earlier on
# PATH is reported as the shadow it is.
report_command_resolution() {
    local installed_binary="$1"

    hash -r 2>/dev/null || true
    local resolved
    resolved="$(command -v "$BINARY_NAME" 2>/dev/null || true)"
    if [[ -z "$resolved" ]]; then
        return 0
    fi
    if [[ "$resolved" -ef "$installed_binary" ]]; then
        print_success "${BINARY_NAME} now runs ${DIM}${resolved}${NC}"
        return 0
    fi
    print_warning "${BINARY_NAME} currently runs ${resolved}, not the build just installed (${installed_binary})."
    print_hint "Put $(dirname "$installed_binary") earlier on your PATH, or remove the other copy."
}

# Make `putnami` usable in the shell that ran the installer, in order:
#   1. the install directory is already on PATH — say so;
#   2. a user-writable directory already on PATH gets an atomically-swapped
#      symlink — no privilege escalation, ever;
#   3. neither — print the exact export line and record it for future shells.
# Whatever happens, the last word is what `putnami` actually resolves to.
ensure_command_available() {
    local installed_binary="$1"

    local install_dir
    install_dir="$(dirname "$installed_binary")"

    if path_contains "$install_dir"; then
        print_success "${install_dir} is already on your PATH"
    elif ! link_into_path_dir "$installed_binary"; then
        configure_shell_path "$install_dir"
    fi

    report_command_resolution "$installed_binary"
}

# The first directory in zsh's fpath that the user can write to, or "".
writable_user_fpath_dir() {
    command -v zsh >/dev/null 2>&1 || return 0
    zsh -c 'for d in $fpath; do
        case "$d" in "$HOME"/*) [[ -d "$d" && -w "$d" ]] && print -r -- "$d" && break ;; esac
    done' 2>/dev/null || true
}

# Install shell completions for the detected shell, using the freshly installed
# binary to generate them. A completion failure is never fatal: the CLI works
# without completions, and this runs after the binary is already in place.
install_shell_completions() {
    local binary_path="$1"
    local shell_name
    shell_name="$(basename "${SHELL:-/bin/sh}")"

    if [[ ! -x "$binary_path" ]]; then
        return 0
    fi

    local comp_file=""
    local comp_dir=""
    local needs_fpath_line=false
    case "$shell_name" in
        zsh)
            # oh-my-zsh's custom completions directory is already in fpath, and
            # so is any writable user directory zsh reported. Only the ~/.zfunc
            # fallback needs a line in .zshrc.
            if [[ -n "${ZSH:-}" && -d "${ZSH}" ]]; then
                comp_dir="${ZSH_CUSTOM:-${ZSH}/custom}/completions"
            else
                comp_dir="$(writable_user_fpath_dir)"
            fi
            if [[ -z "$comp_dir" ]]; then
                comp_dir="${HOME}/.zfunc"
                needs_fpath_line=true
            fi
            comp_file="${comp_dir}/_putnami"
            ;;
        bash)
            comp_dir="${HOME}/.local/share/bash-completion/completions"
            comp_file="${comp_dir}/${BINARY_NAME}"
            ;;
        fish)
            comp_dir="${HOME}/.config/fish/completions"
            comp_file="${comp_dir}/${BINARY_NAME}.fish"
            ;;
        *)
            # Unknown shell — nothing to generate for.
            return 0
            ;;
    esac

    local action="installed"
    [[ -f "$comp_file" ]] && action="updated"

    mkdir -p "$comp_dir" 2>/dev/null || return 0

    # Stage then rename, as everywhere else in this script. Redirecting straight
    # into $comp_file truncates it before the binary runs, so a re-install whose
    # completion regen fails silently destroys completions that worked.
    local staged="${comp_file}.new.$$"
    if ! "$binary_path" completion "$shell_name" > "$staged" 2>/dev/null; then
        rm -f "$staged"
        return 0
    fi
    mv -f "$staged" "$comp_file"
    print_success "Shell completions ${action} ${DIM}(${comp_file})${NC}"

    if [[ "$shell_name" == "zsh" ]]; then
        if [[ "$needs_fpath_line" == true ]]; then
            local fpath_line="fpath=(${comp_dir} \$fpath)"
            append_once "${ZDOTDIR:-$HOME}/.zshrc" "$fpath_line" \
                "${fpath_line}
autoload -Uz compinit && compinit" || true
        fi
        print_info "Restart zsh or run: rm -f ~/.zcompdump* && exec zsh"
    fi
}

# Register Putnami's stable installed path with agent hosts through each host's
# supported CLI. Host configuration is human-owned: an existing `putnami`
# definition is inspected and preserved byte-for-byte, even when it points at a
# different command. This is intentionally best-effort because managed host
# policy may prohibit user configuration while still allowing the CLI install.
claude_mcp_definition_matches() {
    local details="$1"
    local installed_binary="$2"
    grep -qxF "  Command: ${installed_binary}" "$details" \
        && grep -qxF "  Args: mcp" "$details" \
        && grep -qxF '    PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}' "$details" \
        && [[ "$(grep -c '^    [^ ]' "$details" 2>/dev/null || true)" == "1" ]]
}

# Compare Codex's structured answer to the launcher contract by meaning, not by
# formatting. `codex mcp get --json` is a host-owned document: it may gain a
# field, reorder keys, or reindent between releases, and none of that turns
# Putnami's own entry into a human override. Whitespace outside string values is
# removed first, then only the fields whose value actually changes behavior are
# asserted. A field Codex has not written at all is absent rather than null, so
# the optional ones accept either.
codex_json_field_absent_or() {
    local compact="$1"
    local field="$2"
    local allowed="$3"
    case "$compact" in
        *"\"${field}\":"*) [[ "$compact" == *"\"${field}\":${allowed}"* ]] ;;
        *) return 0 ;;
    esac
}

codex_mcp_definition_matches() {
    local details="$1"
    local json_details="$2"
    local installed_binary="$3"
    grep -qxF "  command: ${installed_binary}" "$details" \
        && grep -qxF "  args: mcp" "$details" \
        && grep -qxF "  cwd: -" "$details" \
        && grep -qxF "  env: -" "$details" || return 1

    local compact expected_command
    compact="$(tr -d ' \t\n' < "$json_details")" || return 1
    # The same squeeze is applied to the expected path, because compaction also
    # removes spaces inside string values. Two install directories differing
    # only in spacing would compare equal here; the cost of that is one extra
    # "verified" line, and the alternative is a false "preserved" for every user
    # whose home directory has a space in it.
    expected_command="$(printf '%s' "$installed_binary" | tr -d ' \t\n')"

    # Load-bearing: a different command or argument is a different launcher; a
    # stored cwd or env defeats per-session workspace resolution; a disabled
    # server or a tool filter is a deliberate human restriction.
    [[ "$compact" == *'"name":"putnami"'* ]] \
        && [[ "$compact" == *"\"command\":\"${expected_command}\""* ]] \
        && [[ "$compact" == *'"args":["mcp"]'* ]] \
        && [[ "$compact" == *'"enabled":true'* ]] \
        && [[ "$compact" == *'"cwd":null'* ]] \
        && codex_json_field_absent_or "$compact" env null \
        && codex_json_field_absent_or "$compact" env_vars '[]' \
        && codex_json_field_absent_or "$compact" enabled_tools null \
        && codex_json_field_absent_or "$compact" disabled_tools null
}

configure_claude_code() {
    local installed_binary="$1"
    command -v claude >/dev/null 2>&1 || return 0

    local details="${TEMP_DIR}/claude-mcp.txt"
    local get_status=0
    claude mcp get putnami >"$details" 2>&1 || get_status=$?
    if claude_mcp_definition_matches "$details" "$installed_binary"; then
        if grep -Eq '^  Status: .*Connected$' "$details"; then
            print_success "Claude Code MCP configuration and connection verified for new sessions"
        else
            print_success "Claude Code MCP configuration verified for new sessions"
            print_info "MCP connection not yet verified; Claude Code will check it when a Putnami workspace opens."
        fi
        return 0
    fi
    if [[ "$get_status" -eq 0 ]] || grep -qxF 'putnami:' "$details"; then
        print_warning "Claude Code already defines a putnami MCP server; its human configuration was preserved."
        print_hint "Inspect the active definition with: claude mcp get putnami"
        return 0
    fi

    if ! claude mcp add --scope user \
        --env 'PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}' \
        --transport stdio putnami -- "$installed_binary" mcp \
        >"$details" 2>&1; then
        print_warning "Could not register Putnami with Claude Code; the CLI installation is still usable."
        print_hint "Claude Code may be managed by host policy. Run: claude mcp get putnami"
        return 0
    fi

    get_status=0
    claude mcp get putnami >"$details" 2>&1 || get_status=$?
    if claude_mcp_definition_matches "$details" "$installed_binary"; then
        if grep -Eq '^  Status: .*Connected$' "$details"; then
            print_success "Claude Code MCP configuration and connection verified for new sessions"
        else
            print_success "Claude Code MCP configuration verified for new sessions"
            print_info "MCP connection not yet verified; Claude Code will check it when a Putnami workspace opens."
        fi
        return 0
    fi
    print_warning "Claude Code accepted the MCP registration but did not report the expected Putnami launcher."
    print_hint "Inspect the active definition with: claude mcp get putnami"
}

configure_codex() {
    local installed_binary="$1"
    command -v codex >/dev/null 2>&1 || return 0

    local details="${TEMP_DIR}/codex-mcp.txt"
    local json_details="${TEMP_DIR}/codex-mcp.json"
    if codex mcp get putnami >"$details" 2>&1; then
        if codex mcp get putnami --json >"$json_details" 2>&1 \
            && codex_mcp_definition_matches "$details" "$json_details" "$installed_binary"; then
            print_success "Codex MCP configuration verified for new sessions"
        else
            print_warning "Codex already defines a putnami MCP server; its human TOML and launcher were preserved."
            print_hint "Inspect the active definition with: codex mcp get putnami"
        fi
        return 0
    fi

    if ! codex mcp add putnami -- "$installed_binary" mcp >"$details" 2>&1; then
        print_warning "Could not register Putnami with Codex; the CLI installation is still usable."
        print_hint "Codex may be managed by host policy. Run: codex mcp get putnami"
        return 0
    fi

    if codex mcp get putnami >"$details" 2>&1 \
        && codex mcp get putnami --json >"$json_details" 2>&1 \
        && codex_mcp_definition_matches "$details" "$json_details" "$installed_binary"; then
        print_success "Codex MCP configuration verified for new sessions"
        return 0
    fi
    print_warning "Codex accepted the MCP registration but did not report the expected Putnami launcher."
    print_hint "Inspect the active definition with: codex mcp get putnami"
}

configure_agent_hosts() {
    local installed_binary="$1"

    if [[ -n "$NO_AGENT_HOSTS" ]]; then
        print_info "Skipping agent-host registration (${NO_AGENT_HOSTS_ENV}/--no-agent-hosts)."
        return 0
    fi

    print_step "Connecting supported agent hosts..."
    configure_claude_code "$installed_binary"
    configure_codex "$installed_binary"
}

# Report the installed CLI, then make it reachable, complete-able, and available
# to supported agent hosts without storing the current repository in a global
# host configuration.
finish_install() {
    local installed_binary="$1"

    print_step "Checking the installed CLI..."

    if [[ ! -x "$installed_binary" ]]; then
        print_warning "Binary installed but not executable: ${installed_binary}"
        return 0
    fi

    local reported
    reported="$("$installed_binary" --version 2>/dev/null | head -n1 | awk '{print $NF}')"
    if [[ -n "$reported" ]]; then
        print_success "${BINARY_NAME} ${reported}"
    else
        print_success "${BINARY_NAME} installed"
    fi

    ensure_command_available "$installed_binary"
    install_shell_completions "$installed_binary"
    configure_agent_hosts "$installed_binary"
}

# Download the command map to <file>. A map larger than COMMAND_MAP_MAX_BYTES
# is refused rather than read.
fetch_command_map() {
    local url="$1"
    local file="$2"

    local curl_err="${TEMP_DIR}/command-map.stderr"
    if ! curl -fsSL --connect-timeout 30 --max-filesize "$COMMAND_MAP_MAX_BYTES" \
        -o "$file" "$url" 2>"$curl_err"; then
        print_error "Could not download the command map from $(redact_url "$url")"
        local line
        while IFS= read -r line; do
            [[ -n "$line" ]] && print_hint "curl: ${line}"
        done < "$curl_err"
        return 1
    fi
}

# Print the extension that provides <command>, as the command map names it.
#
# The whole map is validated before any of it is used: a missing header, a
# malformed line anywhere, or a command listed twice refuses the run, so what a
# command resolves to never depends on where in the file a mistake sits. No
# value from the map is ever evaluated; it is only passed as one argument.
resolve_run_extension() {
    local map_file="$1"
    local command="$2"
    local map_label="$3"

    local line=""
    local content=""
    local line_number=0
    local header_seen=false
    local listed=$'\n'
    local resolved=""
    local entry_command=""
    local entry_ref=""
    while IFS= read -r line || [[ -n "$line" ]]; do
        line_number=$((line_number + 1))
        if [[ "$header_seen" == false ]]; then
            if [[ "$line" != "$COMMAND_MAP_HEADER" ]]; then
                print_error "${map_label} is not a command map: its first line is not ${COMMAND_MAP_HEADER}. Refusing to run anything."
                return 1
            fi
            header_seen=true
            continue
        fi
        # Blank lines and comments are ignored, indented or not. Blanks are
        # spaces and tabs only: a carriage return still makes a line malformed.
        content="${line#"${line%%[!${BLANKS}]*}"}"
        case "$content" in
            "" | "#"*) continue ;;
        esac
        if [[ ! "$line" =~ $COMMAND_MAP_ENTRY_PATTERN ]]; then
            print_error "${map_label} line ${line_number} is not \"<command> <@scope/name[@constraint]>\". Refusing to run anything."
            return 1
        fi
        entry_command="${BASH_REMATCH[1]}"
        entry_ref="${BASH_REMATCH[2]}"
        if [[ ! "$entry_command" =~ $RUN_COMMAND_PATTERN ]]; then
            print_error "${map_label} line ${line_number} names a command that is not ^[a-z][a-z0-9-]{0,63}\$. Refusing to run anything."
            return 1
        fi
        if [[ ! "$entry_ref" =~ $EXTENSION_REF_PATTERN ]]; then
            print_error "${map_label} line ${line_number} names an extension that is not @scope/name[@constraint]. Refusing to run anything."
            return 1
        fi
        case "$listed" in
            *$'\n'"${entry_command}"$'\n'*)
                print_error "${map_label} lists ${entry_command} twice (line ${line_number}). Refusing to guess which extension provides it."
                return 1
                ;;
        esac
        listed="${listed}${entry_command}"$'\n'
        if [[ "$entry_command" == "$command" ]]; then
            resolved="$entry_ref"
        fi
    done < "$map_file"

    if [[ "$header_seen" == false ]]; then
        print_error "${map_label} is empty, not a command map. Refusing to run anything."
        return 1
    fi
    if [[ -z "$resolved" ]]; then
        print_error "putnami ${command} is not a command the installer can run: ${map_label} does not list it."
        print_hint "Nothing was installed. To install the CLI alone: curl -fsSL ${INSTALL_SCRIPT_URL} | bash"
        return 1
    fi
    printf '%s' "$resolved"
}

# Pin the extension that provides <command> for the current user, then run
# `putnami <command>` in the caller's directory and exit with its status.
#
# The pin passes --latest, so re-running the one line moves an existing pin to
# the newest release the map entry allows instead of keeping the first one.
#
# The command's stdin is the terminal when one can be opened, and /dev/null
# otherwise; never this script's own stdin, which under `curl | bash` is the
# pipe the script arrives on. Its stdout is the caller's stdout (fd 3), because
# the installer's own messages go to stderr in this mode.
run_requested_command() {
    local binary="$1"
    local command="$2"
    local extension_ref="$3"

    echo ""
    print_step "Pinning ${extension_ref} for your user..."
    local status=0
    "$binary" extensions install --user --latest "$extension_ref" </dev/null 3>&- || status=$?
    if [[ "$status" -ne 0 ]]; then
        print_error "putnami extensions install --user --latest ${extension_ref} failed (exit ${status}); putnami ${command} was not run."
        print_hint "The CLI itself is installed: ${binary}"
        exit "$status"
    fi
    print_success "Pinned ${extension_ref}"

    local command_stdin=/dev/null
    if (exec </dev/tty) 2>/dev/null; then
        command_stdin=/dev/tty
    fi

    echo ""
    print_step "Running putnami ${command} in ${PWD}..."
    echo ""
    status=0
    "$binary" "$command" <"$command_stdin" >&3 3>&- || status=$?
    exit "$status"
}

# Print completion footer
print_footer() {
    echo ""
    echo -e "${SPARKLE} ${BOLD}${GREEN}Done${NC}"
    echo ""
    echo -e "${BOLD}Next:${NC}"
    if [[ -n "$PATH_LINE_TO_RUN" ]]; then
        printf '  %b%s%b\n' "$CYAN" "$PATH_LINE_TO_RUN" "$NC"
    fi
    echo -e "  ${CYAN}putnami --help${NC}"
    echo -e "  ${CYAN}putnami init${NC}"
    echo ""
    echo -e "${DIM}Docs:   putnami.dev${NC}"
    echo -e "${DIM}GitHub: github.com/putnami/putnami${NC}"
    echo ""
}

print_usage() {
    echo "Putnami CLI Installer"
    echo ""
    echo "Usage: curl -fsSL ${INSTALL_SCRIPT_URL} | bash"
    echo "       curl -fsSL \"${INSTALL_SCRIPT_URL}?run=<command>\" | bash"
    echo "       ./install.sh [options]"
    echo ""
    echo "Options:"
    echo "  --version <tag>       Install a specific version or channel (default: latest)"
    echo "  --install-dir <dir>   Install directory (overrides the versioned layout)"
    echo "  --variant <go|ts>     CLI variant to install (default: go)"
    echo "  --download-url <url>  Direct URL to a binary asset (bypasses the registry)"
    echo "  --sha256 <hex>        Expected SHA-256 of that asset (required with --download-url)"
    echo "  --no-agent-hosts      Do not register Putnami with Claude Code or Codex"
    echo "  --run <command>       Then pin the extension that provides <command> for your"
    echo "                        user and run putnami <command> in the current directory"
    echo "  --help, -h            Show this help message"
    echo ""
    # printf, not hand-counted spaces: the longest name here is 33 characters,
    # and every past attempt to pad these by eye left one row out of column.
    echo "Environment:"
    printf '  %-33s %s\n' "PUTNAMI_INSTALL_DIR" "same as --install-dir"
    printf '  %-33s %s\n' "PUTNAMI_VERSION" "same as --version"
    printf '  %-33s %s\n' "PUTNAMI_VARIANT" "same as --variant"
    printf '  %-33s %s\n' "PUTNAMI_DOWNLOAD_URL" "same as --download-url"
    printf '  %-33s %s\n' "${EXPECTED_SHA256_ENV}" "same as --sha256"
    printf '  %-33s %s\n' "PUTNAMI_REGISTRY_URL" "registry base (default ${DEFAULT_REGISTRY_URL})"
    printf '  %-33s %s\n' "${ALLOW_INSECURE_REGISTRY_ENV}=1" "accept a plaintext http:// registry"
    printf '  %-33s %s\n' "${UNSAFE_INSTALL_ENV}=1" "install without integrity verification"
    printf '  %-33s %s\n' "${NO_AGENT_HOSTS_ENV}=1" "same as --no-agent-hosts"
    printf '  %-33s %s\n' "${RUN_ENV}" "same as --run"
    printf '  %-33s %s\n' "${COMMAND_MAP_URL_ENV}" "command map for --run (default ${DEFAULT_COMMAND_MAP_URL})"
    printf '  %-33s %s\n' "NO_COLOR" "disable colored output"
    echo ""
    echo "The installer never escalates privileges: it writes only where you can"
    echo "already write, and refuses a download it cannot verify."
    echo ""
    echo "With --run, the command map names the extension that provides <command>; a"
    echo "command it does not list installs nothing. The installer's messages go to"
    echo "stderr, the command's output to stdout, and the exit status is the command's."
    echo "Nothing is written to the current directory."
    echo ""
    echo "Supported platforms: ${SUPPORTED_MATRIX}."
    echo "On Windows, in a PowerShell window: irm ${INSTALL_PS1_URL} | iex"
}

# Main installation flow
main() {
    local VERSION="${PUTNAMI_VERSION:-latest}"
    local TAG=""
    local OS=""
    local ARCH=""
    local INSTALL_DIR="${PUTNAMI_INSTALL_DIR:-}"
    local DOWNLOAD_URL="${PUTNAMI_DOWNLOAD_URL:-}"
    local REGISTRY_URL="${PUTNAMI_REGISTRY_URL:-$DEFAULT_REGISTRY_URL}"
    local VARIANT="${PUTNAMI_VARIANT:-go}"
    local EXPECTED_SHA256="${PUTNAMI_EXPECTED_SHA256:-}"
    local RUN_COMMAND="${PUTNAMI_RUN:-$RUN_COMMAND_DEFAULT}"
    local COMMAND_MAP_URL="${PUTNAMI_COMMAND_MAP_URL:-$DEFAULT_COMMAND_MAP_URL}"
    local RUN_EXTENSION=""
    local ASSET_FILE=""
    local HEADERS_FILE=""
    local SOURCE_LABEL=""
    local BINARY_SOURCE=""
    local INSTALLED_BINARY=""

    TAG="$(normalize_tag "$VERSION")"
    OS="$(detect_os)"
    ARCH="$(detect_arch)"

    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            --version)
                if [[ -z "$2" ]]; then
                    print_error "--version requires a value"
                    exit 1
                fi
                VERSION="$2"
                TAG="$(normalize_tag "$VERSION")"
                shift 2
                ;;
            --install-dir)
                if [[ -z "$2" ]]; then
                    print_error "--install-dir requires a value"
                    exit 1
                fi
                INSTALL_DIR="$2"
                shift 2
                ;;
            --download-url)
                if [[ -z "$2" ]]; then
                    print_error "--download-url requires a value"
                    exit 1
                fi
                DOWNLOAD_URL="$2"
                shift 2
                ;;
            --sha256)
                if [[ -z "$2" ]]; then
                    print_error "--sha256 requires a value"
                    exit 1
                fi
                EXPECTED_SHA256="$2"
                shift 2
                ;;
            --variant)
                if [[ -z "$2" ]]; then
                    print_error "--variant requires a value (go or ts)"
                    exit 1
                fi
                VARIANT="$2"
                shift 2
                ;;
            --no-agent-hosts)
                NO_AGENT_HOSTS=1
                shift
                ;;
            --run)
                if [[ -z "$2" ]]; then
                    print_error "--run requires a command"
                    exit 1
                fi
                RUN_COMMAND="$2"
                shift 2
                ;;
            --help|-h)
                print_usage
                exit 0
                ;;
            *)
                print_error "Unknown option: $1"
                print_hint "Run with --help for the supported options."
                exit 1
                ;;
        esac
    done

    # Checked after parsing so PUTNAMI_VARIANT is held to the same rule as
    # --variant. The variant becomes part of the installed filename
    # (putnami-<variant>-<tag>), so an unchecked typo installs a binary under a
    # name neither the symlink nor `putnami version use` resolves.
    case "$VARIANT" in
        go | ts) ;;
        *)
            print_error "Unknown variant \"${VARIANT}\" (expected go or ts)"
            exit 1
            ;;
    esac

    # Run mode: stdout belongs to the command, so the installer's own messages
    # go to stderr and fd 3 keeps the caller's stdout for the command. The
    # command is checked before anything else, including the network.
    if [[ -n "$RUN_COMMAND" ]]; then
        exec 3>&1 1>&2
        configure_colors 2
        if [[ ! "$RUN_COMMAND" =~ $RUN_COMMAND_PATTERN ]]; then
            print_error "The command to run (--run, ${RUN_ENV}, or ?run=) must match ^[a-z][a-z0-9-]{0,63}\$. Nothing was installed."
            exit 1
        fi
    fi

    require_supported_platform "$OS" "$ARCH"
    check_prerequisites

    # An explicit --install-dir installs under the plain name, for callers that
    # manage the directory themselves. Otherwise the versioned ~/.putnami/bin
    # layout is used, so `putnami version use` and `upgrade` can switch between
    # installs later.
    local USE_VERSIONED=true
    if [[ -n "$INSTALL_DIR" ]]; then
        USE_VERSIONED=false
    else
        INSTALL_DIR="${HOME}/.putnami/bin"
    fi

    # Validate the source before touching the filesystem: a bad URL is a typo to
    # fix, not a reason to create directories.
    REGISTRY_URL="${REGISTRY_URL%/}"
    if [[ -n "$DOWNLOAD_URL" ]]; then
        validate_registry_url "$DOWNLOAD_URL" "download URL"
    else
        validate_registry_url "$REGISTRY_URL" "registry URL"
    fi
    if [[ -n "$RUN_COMMAND" ]]; then
        validate_registry_url "$COMMAND_MAP_URL" "command map URL"
    fi

    TEMP_DIR="$(mktemp -d)"
    ASSET_FILE="${TEMP_DIR}/${BINARY_NAME}.asset"
    HEADERS_FILE="${TEMP_DIR}/response.headers"
    trap 'rm -rf "$TEMP_DIR"' EXIT

    # A command the map does not list is a typo to fix, not a reason to create
    # directories or install a CLI: it is resolved before anything is written.
    if [[ -n "$RUN_COMMAND" ]]; then
        local map_label
        map_label="$(redact_url "$COMMAND_MAP_URL")"
        echo ""
        print_step "Resolving putnami ${RUN_COMMAND}..."
        print_detail "Command map: ${map_label}"
        fetch_command_map "$COMMAND_MAP_URL" "${TEMP_DIR}/command-map.txt" || exit 1
        RUN_EXTENSION="$(resolve_run_extension "${TEMP_DIR}/command-map.txt" "$RUN_COMMAND" "$map_label")" || exit 1
        print_success "putnami ${RUN_COMMAND} is provided by ${RUN_EXTENSION}"
    fi

    ensure_writable_install_dir "$INSTALL_DIR"

    echo ""

    print_step "Downloading Putnami CLI..."

    local download_target=""
    if [[ -n "$DOWNLOAD_URL" ]]; then
        SOURCE_LABEL="the download URL"
        download_target="$DOWNLOAD_URL"
    else
        SOURCE_LABEL="the registry"
        download_target="${REGISTRY_URL}/putnami/cli/download?channel=${TAG}&os=${OS}&arch=${ARCH}"
    fi

    local display_source
    display_source="$(redact_url "$download_target")"
    display_source="${display_source#https://}"
    display_source="${display_source#http://}"

    print_detail "Platform: ${OS}/${ARCH}"
    print_detail "Channel:  ${TAG}"
    print_detail "Source:   ${display_source}"
    print_detail "Target:   ${INSTALL_DIR}"

    local curl_err="${TEMP_DIR}/curl.stderr"
    if ! curl -fsSL --connect-timeout 30 -D "$HEADERS_FILE" -o "$ASSET_FILE" "$download_target" 2>"$curl_err"; then
        if [[ -n "$DOWNLOAD_URL" ]]; then
            print_error "Could not download $(redact_url "$DOWNLOAD_URL")"
        else
            print_error "No compatible release artifact found for ${OS}/${ARCH} (version: ${TAG})"
            print_hint "Registry: $(redact_url "$download_target")"
            print_hint "Set PUTNAMI_DOWNLOAD_URL or use --download-url to provide an explicit asset URL."
        fi
        local line
        while IFS= read -r line; do
            [[ -n "$line" ]] && print_hint "curl: ${line}"
        done < "$curl_err"
        exit 1
    fi

    print_success "Downloaded"

    local advertised_integrity
    advertised_integrity="$(read_advertised_integrity "$HEADERS_FILE")"
    local resolved_version
    resolved_version="$(trim "$(read_header "$HEADERS_FILE" "X-Resolved-Version")")"

    verify_download_integrity "$ASSET_FILE" "$advertised_integrity" "$EXPECTED_SHA256" "$SOURCE_LABEL"

    BINARY_SOURCE="$(extract_binary "$ASSET_FILE" "$TEMP_DIR")"
    chmod +x "$BINARY_SOURCE"

    # Bind the install to an exact version before anything lands in the install
    # directory, so a refusal leaves nothing behind.
    verify_binary_stamp "$BINARY_SOURCE" "$resolved_version"

    echo ""

    print_step "Installing Putnami CLI..."

    if [[ "$USE_VERSIONED" == true ]]; then
        INSTALLED_BINARY="$(install_versioned_binary "$BINARY_SOURCE" "$INSTALL_DIR" "$VARIANT" "$TAG")"
        print_success "Installed to ${INSTALLED_BINARY}"
        print_success "Active: putnami -> $(basename "$INSTALLED_BINARY")"
    else
        INSTALLED_BINARY="$(install_binary "$BINARY_SOURCE" "$INSTALL_DIR")"
        print_success "Installed to ${INSTALLED_BINARY}"
    fi

    echo ""

    finish_install "${INSTALL_DIR}/${BINARY_NAME}"

    if [[ -n "$RUN_COMMAND" ]]; then
        run_requested_command "${INSTALL_DIR}/${BINARY_NAME}" "$RUN_COMMAND" "$RUN_EXTENSION"
    fi

    print_footer
}

# Run main
main "$@"
