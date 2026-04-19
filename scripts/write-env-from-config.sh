#!/usr/bin/env sh
set -eu

if ! command -v python3 >/dev/null 2>&1; then
  echo "error: python3 is required to parse Codex/Claude config files" >&2
  exit 127
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
export CCP_REPO_ROOT="$REPO_ROOT"

python3 - "$@" <<'PY'
import argparse
import json
import os
import re
import secrets
import sys
from pathlib import Path
from urllib.parse import urlparse


def first_non_empty(*values: str) -> str:
    for value in values:
        if value is None:
            continue
        text = str(value).strip()
        if text:
            return text
    return ""


def normalize_backend_base_url(raw: str) -> str:
    base = (raw or "").strip().rstrip("/")
    if not base:
        return ""

    for suffix in (
        "/v1/responses",
        "/v1/messages/count_tokens",
        "/v1/messages",
        "/messages/count_tokens",
        "/messages",
        "/anthropic",
        "/v1",
    ):
        if base.lower().endswith(suffix.lower()):
            base = base[: -len(suffix)].rstrip("/")

    return base


def is_loopback_url(raw: str) -> bool:
    if not (raw or "").strip():
        return False
    try:
        host = (urlparse(raw).hostname or "").strip().lower()
    except Exception:
        return False
    return host in {"localhost", "127.0.0.1", "::1"}


def remove_toml_inline_comment(line: str) -> str:
    in_single = False
    in_double = False
    escaped = False
    for index, ch in enumerate(line):
        if ch == "'" and not in_double:
            in_single = not in_single
            escaped = False
            continue
        if ch == '"' and not in_single and not escaped:
            in_double = not in_double
            escaped = False
            continue
        if ch == "#" and not in_single and not in_double:
            return line[:index]
        escaped = ch == "\\" and not escaped
        if ch != "\\":
            escaped = False
    return line


def unquote_toml_value(raw: str) -> str:
    value = remove_toml_inline_comment(raw).strip()
    if len(value) >= 2 and ((value[0] == value[-1] == '"') or (value[0] == value[-1] == "'")):
        value = value[1:-1]
    return value.strip()


def read_json(path: Path):
    try:
        with path.open("r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError:
        return None
    except Exception as exc:
        print(f"warning: failed to parse JSON file {path}: {exc}", file=sys.stderr)
        return None


def read_codex_config(root: Path) -> dict:
    result = {
        "BackendBaseURL": "",
        "BackendModel": "",
        "BackendAPIKey": "",
    }

    config_path = root / "config.toml"
    auth_path = root / "auth.json"

    if config_path.exists():
        model_provider = ""
        model = ""
        providers = {}
        section = ""

        try:
            lines = config_path.read_text(encoding="utf-8").splitlines()
        except Exception as exc:
            print(f"warning: failed to read Codex config {config_path}: {exc}", file=sys.stderr)
            lines = []

        for line in lines:
            trimmed = remove_toml_inline_comment(line).strip()
            if not trimmed:
                continue

            provider_match = re.match(r"^\s*\[model_providers\.(?:\"([^\"]+)\"|'([^']+)'|([^\]]+))\]\s*$", trimmed)
            if provider_match:
                name = first_non_empty(provider_match.group(1), provider_match.group(2), provider_match.group(3)).strip()
                section = f"model_providers.{name}"
                providers.setdefault(name, {"BaseURL": "", "WireAPI": ""})
                continue

            section_match = re.match(r"^\s*\[(.+)\]\s*$", trimmed)
            if section_match:
                section = section_match.group(1).strip()
                continue

            assignment = re.match(r"^\s*([A-Za-z0-9_-]+)\s*=\s*(.+)$", trimmed)
            if not assignment:
                continue

            key = assignment.group(1)
            value = unquote_toml_value(assignment.group(2))

            if not section:
                if key == "model_provider":
                    model_provider = value
                elif key == "model":
                    model = value
                continue

            if section.startswith("model_providers."):
                provider_name = section[len("model_providers.") :]
                providers.setdefault(provider_name, {"BaseURL": "", "WireAPI": ""})
                if key == "base_url":
                    providers[provider_name]["BaseURL"] = value
                elif key == "wire_api":
                    providers[provider_name]["WireAPI"] = value

        selected_provider = first_non_empty(model_provider, "codex")
        for provider_name in (selected_provider, "codex"):
            provider = providers.get(provider_name)
            if not provider:
                continue
            wire_api = provider.get("WireAPI", "").strip().lower()
            if wire_api and wire_api != "responses":
                continue
            base_url = normalize_backend_base_url(provider.get("BaseURL", ""))
            if base_url:
                result["BackendBaseURL"] = base_url
                result["BackendModel"] = model.strip()
                break

    auth = read_json(auth_path)
    if isinstance(auth, dict):
        result["BackendAPIKey"] = str(auth.get("OPENAI_API_KEY", "")).strip()

    return result


def read_claude_config(home: Path) -> dict:
    result = {
        "BackendBaseURL": "",
        "BackendModel": "",
        "BackendAPIKey": "",
        "ClientAPIKey": "",
    }

    for path in (home / ".claude" / "settings.local.json", home / ".claude" / "settings.json"):
        settings = read_json(path)
        if not isinstance(settings, dict):
            continue
        env = settings.get("env")
        if not isinstance(env, dict):
            continue

        auth_token = str(env.get("ANTHROPIC_AUTH_TOKEN", "")).strip()
        base_url = normalize_backend_base_url(str(env.get("ANTHROPIC_BASE_URL", "")))
        if is_loopback_url(base_url):
            result["ClientAPIKey"] = first_non_empty(result["ClientAPIKey"], auth_token)
            continue

        result["BackendBaseURL"] = first_non_empty(result["BackendBaseURL"], base_url)
        result["BackendAPIKey"] = first_non_empty(result["BackendAPIKey"], auth_token)
        result["BackendModel"] = first_non_empty(result["BackendModel"], str(env.get("ANTHROPIC_MODEL", "")))

    return result


def dotenv_line(key: str, value: str) -> str:
    clean = (value or "").replace("\r", "").replace("\n", "")
    return f"{key}={clean}"


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate .env.local from local Codex/Claude config.")
    parser.add_argument("-o", "--output", default=".env.local", help="Output dotenv path. Relative paths are resolved from the repo root.")
    parser.add_argument("--codex-home", default="", help="Codex config directory. Defaults to CODEX_HOME or ~/.codex.")
    parser.add_argument("--home-dir", default="", help="Home directory containing .claude. Defaults to the current user's home.")
    parser.add_argument("-f", "--force", action="store_true", help="Overwrite the output file if it already exists.")
    parser.add_argument("--no-generate-client-key", action="store_true", help="Do not generate CLAUDE_CODE_PROXY_CLIENT_API_KEY when missing.")
    args = parser.parse_args()

    repo_root = Path(os.environ.get("CCP_REPO_ROOT", ".")).resolve()
    output = Path(args.output)
    if not output.is_absolute():
        output = repo_root / output
    output = output.resolve()

    home = Path(first_non_empty(args.home_dir, os.environ.get("USERPROFILE", ""), os.environ.get("HOME", ""), str(Path.home()))).expanduser()
    codex_home = Path(first_non_empty(args.codex_home, os.environ.get("CODEX_HOME", ""), str(home / ".codex"))).expanduser()

    codex = read_codex_config(codex_home)
    claude = read_claude_config(home)

    backend_base_url = first_non_empty(os.environ.get("CLAUDE_CODE_PROXY_BACKEND_BASE_URL", ""), codex["BackendBaseURL"], claude["BackendBaseURL"])
    backend_api_key = first_non_empty(os.environ.get("CLAUDE_CODE_PROXY_BACKEND_API_KEY", ""), codex["BackendAPIKey"], claude["BackendAPIKey"])
    backend_model = first_non_empty(os.environ.get("CLAUDE_CODE_PROXY_BACKEND_MODEL", ""), codex["BackendModel"], claude["BackendModel"], "gpt-5.4")
    client_api_key = first_non_empty(os.environ.get("CLAUDE_CODE_PROXY_CLIENT_API_KEY", ""), claude["ClientAPIKey"])
    if not client_api_key and not args.no_generate_client_key:
        client_api_key = secrets.token_urlsafe(32)

    missing = []
    if not backend_base_url:
        missing.append("CLAUDE_CODE_PROXY_BACKEND_BASE_URL")
    if not backend_api_key:
        missing.append("CLAUDE_CODE_PROXY_BACKEND_API_KEY")
    if not backend_model:
        missing.append("CLAUDE_CODE_PROXY_BACKEND_MODEL")
    if not client_api_key:
        missing.append("CLAUDE_CODE_PROXY_CLIENT_API_KEY")
    if missing:
        print(f"warning: missing required values: {', '.join(missing)}", file=sys.stderr)
        print("warning: set them in your shell or edit the generated env file before starting Docker.", file=sys.stderr)

    if output.exists() and not args.force:
        print(f"error: output file already exists: {output}. Re-run with --force to overwrite.", file=sys.stderr)
        return 2

    lines = [
        "# Generated by scripts/write-env-from-config.sh",
        "# Do not commit this file. It may contain API keys.",
        "",
        dotenv_line("CLAUDE_CODE_PROXY_BACKEND_BASE_URL", backend_base_url),
        dotenv_line("CLAUDE_CODE_PROXY_BACKEND_API_KEY", backend_api_key),
        dotenv_line("CLAUDE_CODE_PROXY_BACKEND_MODEL", backend_model),
        dotenv_line("CLAUDE_CODE_PROXY_CLIENT_API_KEY", client_api_key),
        "",
        "# Optional defaults for Docker/local use.",
        "CLAUDE_CODE_PROXY_BACKEND_PATH=",
        "CLAUDE_CODE_PROXY_REQUEST_TIMEOUT=",
        "CLAUDE_CODE_PROXY_ANTHROPIC_MODEL_ALIAS=",
        "CLAUDE_CODE_PROXY_BACKEND_REASONING_EFFORT=",
        "CLAUDE_CODE_PROXY_ANTHROPIC_API_KEY=",
        "CLAUDE_CODE_PROXY_ANTHROPIC_API_BASE_URL=",
        "CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA=",
        "CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=",
        "CLAUDE_CODE_PROXY_USER_METADATA_ALLOWLIST=",
        "CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA=",
        "CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY=",
        "CLAUDE_CODE_PROXY_ANONYMOUS_MODE=",
        "CLAUDE_CODE_PROXY_ENABLE_MODEL_CAPABILITY_INIT=",
        "CLAUDE_CODE_PROXY_ENABLE_PHASE_COMMENTARY=",
        "CLAUDE_CODE_PROXY_DISABLE_BACKEND_STREAMING=",
        "CLAUDE_CODE_PROXY_DEBUG=",
    ]

    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text("\n".join(lines) + "\n", encoding="utf-8")

    print(f"Wrote {output}")
    print(f"Backend base URL: {backend_base_url}")
    print(f"Backend model: {backend_model}")
    print(f"Backend API key: {'set' if backend_api_key else 'missing'}")
    print(f"Client API key: {'set' if client_api_key else 'missing'}")
    return 0


raise SystemExit(main())
PY
