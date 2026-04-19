param(
    [string]$OutputPath = ".env.local",
    [string]$CodexHome = "",
    [string]$HomeDir = "",
    [switch]$Force,
    [switch]$NoGenerateClientKey
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"

$repoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
if (-not [System.IO.Path]::IsPathRooted($OutputPath)) {
    $OutputPath = Join-Path $repoRoot $OutputPath
}
$OutputPath = [System.IO.Path]::GetFullPath($OutputPath)

function First-NonEmpty {
    foreach ($value in $args) {
        if (-not [string]::IsNullOrWhiteSpace($value)) {
            return $value.Trim()
        }
    }
    return ""
}

function Normalize-BackendBaseUrl {
    param([string]$Raw)

    $base = ($Raw + "").Trim().TrimEnd("/")
    if ($base -eq "") {
        return ""
    }

    $suffixes = @(
        "/v1/responses",
        "/v1/messages/count_tokens",
        "/v1/messages",
        "/messages/count_tokens",
        "/messages",
        "/anthropic",
        "/v1"
    )

    foreach ($suffix in $suffixes) {
        if ($base.EndsWith($suffix, [System.StringComparison]::OrdinalIgnoreCase)) {
            $base = $base.Substring(0, $base.Length - $suffix.Length).TrimEnd("/")
        }
    }

    return $base
}

function Test-LoopbackUrl {
    param([string]$Raw)

    if ([string]::IsNullOrWhiteSpace($Raw)) {
        return $false
    }

    try {
        $uri = [System.Uri]$Raw
    } catch {
        return $false
    }

    $hostName = ($uri.Host + "").Trim().ToLowerInvariant()
    return $hostName -eq "localhost" -or $hostName -eq "127.0.0.1" -or $hostName -eq "::1"
}

function Remove-TomlInlineComment {
    param([string]$Line)

    $inSingle = $false
    $inDouble = $false
    for ($i = 0; $i -lt $Line.Length; $i++) {
        $ch = $Line[$i]
        if ($ch -eq "'" -and -not $inDouble) {
            $inSingle = -not $inSingle
            continue
        }
        if ($ch -eq '"' -and -not $inSingle) {
            $escaped = $i -gt 0 -and $Line[$i - 1] -eq "\"
            if (-not $escaped) {
                $inDouble = -not $inDouble
            }
            continue
        }
        if ($ch -eq "#" -and -not $inSingle -and -not $inDouble) {
            return $Line.Substring(0, $i)
        }
    }
    return $Line
}

function Unquote-TomlValue {
    param([string]$Raw)

    $value = (Remove-TomlInlineComment $Raw).Trim()
    if ($value.Length -ge 2) {
        if (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'"))) {
            $value = $value.Substring(1, $value.Length - 2)
        }
    }
    return $value.Trim()
}

function Get-ObjectPropertyValue {
    param(
        [object]$InputObject,
        [string]$Name
    )

    if ($null -eq $InputObject -or [string]::IsNullOrWhiteSpace($Name)) {
        return $null
    }

    if ($InputObject -is [System.Collections.IDictionary]) {
        if ($InputObject.Contains($Name)) {
            return $InputObject[$Name]
        }
        return $null
    }

    $property = $InputObject.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }

    return $property.Value
}

function Read-CodexConfig {
    param([string]$Root)

    $result = @{
        BackendBaseURL = ""
        BackendModel = ""
        BackendAPIKey = ""
    }

    $configPath = Join-Path $Root "config.toml"
    $authPath = Join-Path $Root "auth.json"

    if (Test-Path -LiteralPath $configPath) {
        $modelProvider = ""
        $model = ""
        $providers = @{}
        $section = ""

        foreach ($line in Get-Content -LiteralPath $configPath) {
            $trimmed = (Remove-TomlInlineComment $line).Trim()
            if ($trimmed -eq "") {
                continue
            }

            if ($trimmed -match '^\s*\[model_providers\.(?:"([^"]+)"|''([^'']+)''|([^\]]+))\]\s*$') {
                $name = First-NonEmpty $Matches[1] $Matches[2] $Matches[3]
                $name = $name.Trim()
                $section = "model_providers.$name"
                if (-not $providers.ContainsKey($name)) {
                    $providers[$name] = @{
                        BaseURL = ""
                        WireAPI = ""
                    }
                }
                continue
            }

            if ($trimmed -match '^\s*\[(.+)\]\s*$') {
                $section = $Matches[1].Trim()
                continue
            }

            if ($trimmed -notmatch '^\s*([A-Za-z0-9_\-]+)\s*=\s*(.+)$') {
                continue
            }

            $key = $Matches[1]
            $value = Unquote-TomlValue $Matches[2]

            if ($section -eq "") {
                switch ($key) {
                    "model_provider" { $modelProvider = $value }
                    "model" { $model = $value }
                }
                continue
            }

            if ($section.StartsWith("model_providers.")) {
                $providerName = $section.Substring("model_providers.".Length)
                if (-not $providers.ContainsKey($providerName)) {
                    $providers[$providerName] = @{
                        BaseURL = ""
                        WireAPI = ""
                    }
                }
                switch ($key) {
                    "base_url" { $providers[$providerName].BaseURL = $value }
                    "wire_api" { $providers[$providerName].WireAPI = $value }
                }
            }
        }

        $selectedProvider = First-NonEmpty $modelProvider "codex"
        foreach ($providerName in @($selectedProvider, "codex")) {
            if (-not $providers.ContainsKey($providerName)) {
                continue
            }
            $provider = $providers[$providerName]
            $wireAPI = ($provider.WireAPI + "").Trim().ToLowerInvariant()
            if ($wireAPI -ne "" -and $wireAPI -ne "responses") {
                continue
            }
            $baseURL = Normalize-BackendBaseUrl $provider.BaseURL
            if ($baseURL -ne "") {
                $result.BackendBaseURL = $baseURL
                $result.BackendModel = $model
                break
            }
        }
    }

    if (Test-Path -LiteralPath $authPath) {
        try {
            $auth = Get-Content -Raw -LiteralPath $authPath | ConvertFrom-Json
            $openAIAPIKey = Get-ObjectPropertyValue -InputObject $auth -Name "OPENAI_API_KEY"
            if ($null -ne $openAIAPIKey) {
                $result.BackendAPIKey = ($openAIAPIKey + "").Trim()
            }
        } catch {
            Write-Warning "Failed to parse Codex auth file: $authPath"
        }
    }

    return $result
}

function Read-ClaudeConfig {
    param([string]$Root)

    $result = @{
        BackendBaseURL = ""
        BackendModel = ""
        BackendAPIKey = ""
        ClientAPIKey = ""
    }

    $paths = @(
        (Join-Path $Root ".claude/settings.local.json"),
        (Join-Path $Root ".claude/settings.json")
    )

    foreach ($path in $paths) {
        if (-not (Test-Path -LiteralPath $path)) {
            continue
        }
        try {
            $settings = Get-Content -Raw -LiteralPath $path | ConvertFrom-Json
        } catch {
            Write-Warning "Failed to parse Claude settings file: $path"
            continue
        }

        $envSettings = Get-ObjectPropertyValue -InputObject $settings -Name "env"
        if ($null -eq $envSettings) {
            continue
        }

        $authToken = ((Get-ObjectPropertyValue -InputObject $envSettings -Name "ANTHROPIC_AUTH_TOKEN") + "").Trim()
        $baseURL = Normalize-BackendBaseUrl ((Get-ObjectPropertyValue -InputObject $envSettings -Name "ANTHROPIC_BASE_URL") + "")
        if (Test-LoopbackUrl $baseURL) {
            $result.ClientAPIKey = First-NonEmpty $result.ClientAPIKey $authToken
            continue
        }

        $result.BackendBaseURL = First-NonEmpty $result.BackendBaseURL $baseURL
        $result.BackendAPIKey = First-NonEmpty $result.BackendAPIKey $authToken
        $result.BackendModel = First-NonEmpty $result.BackendModel ((Get-ObjectPropertyValue -InputObject $envSettings -Name "ANTHROPIC_MODEL") + "")
    }

    return $result
}

function New-RandomToken {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    return [Convert]::ToBase64String($bytes).TrimEnd("=").Replace("+", "-").Replace("/", "_")
}

function Format-DotenvLine {
    param([string]$Key, [string]$Value)

    $clean = ($Value + "").Replace("`r", "").Replace("`n", "")
    return "$Key=$clean"
}

if ($HomeDir -eq "") {
    $HomeDir = First-NonEmpty $env:USERPROFILE $env:HOME
}
if ($CodexHome -eq "") {
    $CodexHome = First-NonEmpty $env:CODEX_HOME (Join-Path $HomeDir ".codex")
}

$codex = Read-CodexConfig $CodexHome
$claude = Read-ClaudeConfig $HomeDir

$backendBaseURL = First-NonEmpty $env:CLAUDE_CODE_PROXY_BACKEND_BASE_URL $codex.BackendBaseURL $claude.BackendBaseURL
$backendAPIKey = First-NonEmpty $env:CLAUDE_CODE_PROXY_BACKEND_API_KEY $codex.BackendAPIKey $claude.BackendAPIKey
$backendModel = First-NonEmpty $env:CLAUDE_CODE_PROXY_BACKEND_MODEL $codex.BackendModel $claude.BackendModel "gpt-5.4"
$clientAPIKey = First-NonEmpty $env:CLAUDE_CODE_PROXY_CLIENT_API_KEY $claude.ClientAPIKey

if ($clientAPIKey -eq "" -and -not $NoGenerateClientKey) {
    $clientAPIKey = New-RandomToken
}

$missing = @()
if ($backendBaseURL -eq "") { $missing += "CLAUDE_CODE_PROXY_BACKEND_BASE_URL" }
if ($backendAPIKey -eq "") { $missing += "CLAUDE_CODE_PROXY_BACKEND_API_KEY" }
if ($backendModel -eq "") { $missing += "CLAUDE_CODE_PROXY_BACKEND_MODEL" }
if ($clientAPIKey -eq "") { $missing += "CLAUDE_CODE_PROXY_CLIENT_API_KEY" }

if ($missing.Count -gt 0) {
    Write-Warning ("Missing required values: " + ($missing -join ", "))
    Write-Warning "Set them in your shell or edit the generated env file before starting Docker."
}

if ((Test-Path -LiteralPath $OutputPath) -and -not $Force) {
    throw "Output file already exists: $OutputPath. Re-run with -Force to overwrite."
}

$lines = @(
    "# Generated by scripts/write-env-from-config.ps1 on $((Get-Date).ToString("yyyy-MM-ddTHH:mm:ssK"))",
    "# Do not commit this file. It may contain API keys.",
    "",
    (Format-DotenvLine "CLAUDE_CODE_PROXY_BACKEND_BASE_URL" $backendBaseURL),
    (Format-DotenvLine "CLAUDE_CODE_PROXY_BACKEND_API_KEY" $backendAPIKey),
    (Format-DotenvLine "CLAUDE_CODE_PROXY_BACKEND_MODEL" $backendModel),
    (Format-DotenvLine "CLAUDE_CODE_PROXY_CLIENT_API_KEY" $clientAPIKey),
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
    "CLAUDE_CODE_PROXY_DEBUG="
)

$outputDir = Split-Path -Parent $OutputPath
if ($outputDir -ne "" -and -not (Test-Path -LiteralPath $outputDir)) {
    New-Item -ItemType Directory -Force -Path $outputDir | Out-Null
}

[System.IO.File]::WriteAllText($OutputPath, ($lines -join [Environment]::NewLine) + [Environment]::NewLine, [System.Text.UTF8Encoding]::new($false))

Write-Host "Wrote $OutputPath"
Write-Host "Backend base URL: $backendBaseURL"
Write-Host "Backend model: $backendModel"
if ($backendAPIKey -ne "") {
    Write-Host "Backend API key: set"
} else {
    Write-Host "Backend API key: missing"
}
if ($clientAPIKey -ne "") {
    Write-Host "Client API key: set"
} else {
    Write-Host "Client API key: missing"
}
