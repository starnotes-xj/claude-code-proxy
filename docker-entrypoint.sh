#!/bin/sh
set -eu

load_env_file() {
    env_file="$1"
    [ -f "$env_file" ] || return 0

    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in
            ''|'#'*) continue ;;
        esac

        key=${line%%=*}
        value=${line#*=}

        key=$(printf '%s' "$key" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        value=$(printf '%s' "$value" | tr -d '\r')

        case "$key" in
            ''|*[!A-Za-z0-9_]*)
                continue
                ;;
        esac

        eval "is_set=\${$key+x}"
        if [ -n "${is_set:-}" ]; then
            continue
        fi

        export "$key=$value"
    done < "$env_file"
}

: "${CLAUDE_CODE_PROXY_ENV_FILE:=/app/.env.local}"

load_env_file "$CLAUDE_CODE_PROXY_ENV_FILE"

: "${CLAUDE_CODE_PROXY_BIN:=/usr/local/bin/claude-codex-proxy}"

exec "$CLAUDE_CODE_PROXY_BIN" "$@"
