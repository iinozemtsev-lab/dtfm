#!/bin/sh
# Pipe-safe POSIX installer for dtfm, lmdtfy, and dtfm-relay.
# Usage: curl -fsSL https://raw.githubusercontent.com/iinozemtsev-lab/dtfm/main/scripts/install.sh | sh
set -eu

main() {
    export LC_ALL=C

    REPO="${DTFM_REPO:-iinozemtsev-lab/dtfm}"
    GITHUB_URL="${DTFM_GITHUB_URL:-https://github.com}"
    DEST="${DTFM_INSTALL_DIR:-}"
    TAG="${DTFM_VERSION:-}"
    FORCE=0
    COMPONENTS=""
    TMP_DIR=""
    STAGE_FILE=""

    usage() {
        cat <<'EOF'
Usage: install.sh [OPTIONS] [COMPONENT...]

Install dtfm, lmdtfy, and/or dtfm-relay static binaries from GitHub Releases
with SHA-256 checksum verification.

Components (default: dtfm lmdtfy):
  dtfm                 Operator terminal approval listener
  lmdtfy               Agent-side command dispatcher
  relay, dtfm-relay    Stateless E2EE WebSocket relay (installed as dtfm-relay)
  all                  Install dtfm, lmdtfy, and dtfm-relay

Options:
  --dir <path>         Target directory (default: /usr/local/bin if root, else ~/.local/bin)
  --version <vX.Y.Z>   Install a specific release tag (default: latest release)
  --repo <owner/name>  GitHub repository (default: iinozemtsev-lab/dtfm)
  --force              Reinstall even if already at target version; replace symlinks
  -h, --help           Show this help message and exit

Environment overrides:
  DTFM_INSTALL_DIR, DTFM_VERSION, DTFM_REPO, DTFM_GITHUB_URL
EOF
    }

    die() {
        printf 'Error: %s\n' "$*" >&2
        exit 1
    }

    has_cmd() {
        command -v "$1" >/dev/null 2>&1
    }

    cleanup() {
        if [ -n "${STAGE_FILE:-}" ]; then
            rm -f -- "$STAGE_FILE" </dev/null >/dev/null 2>&1 || true
        fi
        if [ -n "${TMP_DIR:-}" ] && [ -d "$TMP_DIR" ]; then
            rm -rf -- "$TMP_DIR" </dev/null >/dev/null 2>&1 || true
        fi
    }

    add_component() {
        case " $COMPONENTS " in
            *" $1 "*) ;;
            *) COMPONENTS="${COMPONENTS:+$COMPONENTS }$1" ;;
        esac
    }

    validate_tag() {
        case "$1" in
            ""|*..*|*[!0-9A-Za-z.-]*)
                return 1
                ;;
        esac
        awk -v t="$1" 'BEGIN { exit (t ~ /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/) ? 0 : 1 }' </dev/null
    }

    validate_repo() {
        case "$1" in
            ""|*/*/*|*..*|*[!0-9A-Za-z._/-]*)
                return 1
                ;;
            */*) ;;
            *) return 1 ;;
        esac
        repo_owner="${1%%/*}"
        repo_name="${1#*/}"
        case "$repo_owner" in
            ""|.|..|[!0-9A-Za-z]*|*[!0-9A-Za-z]) return 1 ;;
        esac
        case "$repo_name" in
            ""|.|..|[!0-9A-Za-z]*|*[!0-9A-Za-z]) return 1 ;;
        esac
        return 0
    }

    validate_github_url() {
        while :; do
            case "$GITHUB_URL" in
                */) GITHUB_URL="${GITHUB_URL%/}" ;;
                *) break ;;
            esac
        done
        case "$GITHUB_URL" in
            https://*|http://*) ;;
            *) die "Invalid DTFM_GITHUB_URL (must start with https:// or http://): $GITHUB_URL" ;;
        esac
        case "$GITHUB_URL" in
            *[[:space:]]*|*[[:cntrl:]]*)
                die "Invalid DTFM_GITHUB_URL (contains whitespace or control characters)."
                ;;
        esac
    }

    require_arg() {
        if [ "$#" -lt 2 ] || [ -z "$2" ]; then
            die "Option $1 requires a non-empty argument."
        fi
    }

    parse_args() {
        parse_flags=1
        while [ "$#" -gt 0 ]; do
            arg="$1"
            if [ "$parse_flags" -eq 1 ]; then
                case "$arg" in
                    -h|--help)
                        usage
                        exit 0
                        ;;
                    --dir)
                        require_arg "$@"
                        DEST="$2"
                        shift 2
                        continue
                        ;;
                    --dir=*)
                        DEST="${arg#--dir=}"
                        [ -n "$DEST" ] || die "Option --dir requires a non-empty argument."
                        shift
                        continue
                        ;;
                    --version)
                        require_arg "$@"
                        TAG="$2"
                        shift 2
                        continue
                        ;;
                    --version=*)
                        TAG="${arg#--version=}"
                        [ -n "$TAG" ] || die "Option --version requires a non-empty argument."
                        shift
                        continue
                        ;;
                    --repo)
                        require_arg "$@"
                        REPO="$2"
                        shift 2
                        continue
                        ;;
                    --repo=*)
                        REPO="${arg#--repo=}"
                        [ -n "$REPO" ] || die "Option --repo requires a non-empty argument."
                        shift
                        continue
                        ;;
                    --force)
                        FORCE=1
                        shift
                        continue
                        ;;
                    --)
                        parse_flags=0
                        shift
                        continue
                        ;;
                    -*)
                        die "Unknown option: $arg (use --help for usage)"
                        ;;
                esac
            fi

            case "$arg" in
                dtfm|lmdtfy)
                    add_component "$arg"
                    ;;
                relay|dtfm-relay)
                    add_component "dtfm-relay"
                    ;;
                all)
                    add_component "dtfm"
                    add_component "lmdtfy"
                    add_component "dtfm-relay"
                    ;;
                *)
                    die "Unknown component: $arg (expected dtfm, lmdtfy, relay, dtfm-relay, or all)"
                    ;;
            esac
            shift
        done

        if [ -z "$COMPONENTS" ]; then
            COMPONENTS="dtfm lmdtfy"
        fi
    }

    detect_platform() {
        raw_os="$(uname -s </dev/null)"
        case "$raw_os" in
            Linux)  OS="linux" ;;
            Darwin) OS="darwin" ;;
            *)      die "Unsupported operating system: $raw_os" ;;
        esac

        raw_arch="$(uname -m </dev/null)"
        case "$raw_arch" in
            x86_64|amd64)  ARCH="amd64" ;;
            aarch64|arm64) ARCH="arm64" ;;
            *)             die "Unsupported architecture: $raw_arch" ;;
        esac
    }

    resolve_dest() {
        if [ -z "$DEST" ]; then
            uid="$(id -u </dev/null)"
            if [ "$uid" -eq 0 ]; then
                DEST="/usr/local/bin"
            else
                [ -n "${HOME:-}" ] || die "HOME is not set; pass --dir <path>."
                DEST="${HOME}/.local/bin"
            fi
        fi
        while :; do
            case "$DEST" in
                /) break ;;
                */) DEST="${DEST%/}" ;;
                *) break ;;
            esac
        done
        [ -n "$DEST" ] || DEST="/"
        case "$DEST" in
            -*) DEST="./$DEST" ;;
        esac
    }

    resolve_latest_tag() {
        latest_url="${GITHUB_URL}/${REPO}/releases/latest"
        if has_cmd curl; then
            effective_url="$(curl -fsSLI --connect-timeout 10 --max-time 30 -o /dev/null -w '%{url_effective}' -- "$latest_url" </dev/null)" || \
                die "Failed to resolve latest release from $latest_url"
        elif has_cmd wget; then
            wget_out="$(wget --spider -S -T 30 -- "$latest_url" </dev/null 2>&1)" || \
                die "Failed to resolve latest release from $latest_url"
            effective_url="$(printf '%s\n' "$wget_out" | awk '
                /^[[:space:]]*[Ll]ocation:[[:space:]]*/ {
                    line = $0
                    sub(/^[[:space:]]*[Ll]ocation:[[:space:]]*/, "", line)
                    sub(/\r$/, "", line)
                    sub(/[[:space:]].*$/, "", line)
                    loc = line
                }
                END { print loc }
            ')"
        else
            die "Neither curl nor wget is installed."
        fi

        effective_url="${effective_url%%\?*}"
        effective_url="${effective_url%%\#*}"
        effective_url="${effective_url%/}"

        case "$effective_url" in
            */releases/tag/*)
                TAG="${effective_url##*/releases/tag/}"
                ;;
            *)
                die "No published release found at $latest_url (redirected to: ${effective_url:-none})."
                ;;
        esac
    }

    download_file() {
        dl_url="$1"
        dl_out="$2"
        if has_cmd curl; then
            curl -fsSL --connect-timeout 10 --max-time 120 -o "$dl_out" -- "$dl_url" </dev/null || \
                die "Failed to download $dl_url"
        elif has_cmd wget; then
            wget -q -T 120 -O "$dl_out" -- "$dl_url" </dev/null || \
                die "Failed to download $dl_url"
        else
            die "Neither curl nor wget is installed."
        fi
    }

    compute_sha256() {
        file_path="$1"
        if has_cmd sha256sum; then
            sha256sum < "$file_path" | awk '{print tolower($1)}'
        elif has_cmd shasum; then
            shasum -a 256 < "$file_path" | awk '{print tolower($1)}'
        elif has_cmd openssl; then
            openssl dgst -sha256 < "$file_path" | awk '{print tolower($NF)}'
        else
            die "No SHA-256 tool found (need sha256sum, shasum, or openssl)."
        fi
    }

    lookup_sha256() {
        sums_file="$1"
        target_asset="$2"
        awk -v target="$target_asset" '
            { sub(/\r$/, "") }
            /^[0-9a-fA-F]{64} [ *]/ {
                if (substr($0, 67) == target) {
                    print tolower(substr($0, 1, 64))
                    found = 1
                    exit 0
                }
            }
            END { if (!found) exit 1 }
        ' "$sums_file" </dev/null
    }

    asset_for_bin() {
        case "$1" in
            dtfm)       printf 'dtfm-%s-%s\n' "$OS" "$ARCH" ;;
            lmdtfy)     printf 'lmdtfy-%s-%s\n' "$OS" "$ARCH" ;;
            dtfm-relay) printf 'relay-%s-%s\n' "$OS" "$ARCH" ;;
            *)          die "Internal error: unknown binary $1" ;;
        esac
    }

    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP

    parse_args "$@"

    validate_github_url
    validate_repo "$REPO" || die "Invalid repository name: $REPO"
    detect_platform
    resolve_dest

    if [ -z "$TAG" ]; then
        resolve_latest_tag
    fi
    validate_tag "$TAG" || die "Invalid release tag: $TAG"

    # Upfront directory guard, symlink guard, and idempotent version check before any downloads.
    TO_INSTALL=""
    for bin in $COMPONENTS; do
        target="$DEST/$bin"
        if [ ! -L "$target" ] && [ -d "$target" ]; then
            die "Refusing to overwrite directory $target."
        fi
        if [ -L "$target" ] && [ "$FORCE" -ne 1 ]; then
            link_dest="$(readlink -- "$target" </dev/null 2>/dev/null || printf 'unknown')"
            die "Refusing to overwrite symlink $target -> $link_dest (pass --force to replace)."
        fi
        if [ "$FORCE" -ne 1 ] && [ -f "$target" ] && [ -x "$target" ]; then
            cur_ver="$("$target" --version </dev/null 2>/dev/null || true)"
            if [ "$cur_ver" = "$bin $TAG" ]; then
                continue
            fi
        fi
        TO_INSTALL="${TO_INSTALL:+$TO_INSTALL }$bin"
    done

    if [ -z "$TO_INSTALL" ]; then
        printf '%s (%s) already up to date in %s\n' "$COMPONENTS" "$TAG" "$DEST"
        exit 0
    fi

    TMP_DIR="$(mktemp -d)" </dev/null
    BASE_URL="${GITHUB_URL}/${REPO}/releases/download/${TAG}"
    SUMS_PATH="${TMP_DIR}/SHA256SUMS"

    printf 'Installing %s (%s for %s/%s) into %s...\n' "$TO_INSTALL" "$TAG" "$OS" "$ARCH" "$DEST"
    download_file "${BASE_URL}/SHA256SUMS" "$SUMS_PATH"

    for bin in $TO_INSTALL; do
        asset="$(asset_for_bin "$bin")"
        bin_tmp="${TMP_DIR}/${asset}"
        download_file "${BASE_URL}/${asset}" "$bin_tmp"

        expected="$(lookup_sha256 "$SUMS_PATH" "$asset")" || \
            die "Checksum entry for $asset not found in SHA256SUMS."
        actual="$(compute_sha256 "$bin_tmp")"
        if [ -z "$actual" ] || [ "$actual" != "$expected" ]; then
            die "SHA-256 mismatch for $asset (expected $expected, got ${actual:-empty})."
        fi
    done

    mkdir -p -- "$DEST" </dev/null
    for bin in $TO_INSTALL; do
        asset="$(asset_for_bin "$bin")"
        STAGE_FILE="$(mktemp "${DEST}/.${bin}.tmp.XXXXXX")" </dev/null
        cp -- "${TMP_DIR}/${asset}" "$STAGE_FILE" </dev/null
        chmod 0755 -- "$STAGE_FILE" </dev/null
        if [ -L "${DEST}/${bin}" ] && [ -d "${DEST}/${bin}" ]; then
            rm -f -- "${DEST}/${bin}" </dev/null
        fi
        mv -f -- "$STAGE_FILE" "${DEST}/${bin}" </dev/null
        STAGE_FILE=""
        printf 'Installed %s -> %s/%s\n' "$bin" "$DEST" "$bin"
    done

    first_bin=""
    for bin in $TO_INSTALL; do
        first_bin="$bin"
        break
    done
    printf 'Verify build provenance: gh attestation verify "%s/%s" --repo %s\n' "$DEST" "$first_bin" "$REPO"

    case ":${PATH:-}:" in
        *":${DEST}:"*) ;;
        *)
            printf 'Add to PATH: export PATH="%s:$PATH"\n' "$DEST"
            ;;
    esac
}

main "$@"
