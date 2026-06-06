# =============================================================================
# AI LOCAL STACK INSTALLER (Windows / PowerShell port of claude.cpp v14)
# llama.cpp + LiteLLM + 16GB-GPU model menu + speculative decoding + launcher
# Targets: Windows 10/11 x64, NVIDIA (CUDA) primary, CPU fallback
# Based on https://github.com/d4rks1d33/claude.cpp
# =============================================================================

[CmdletBinding()]
param(
    [switch]$NoBuild,
    [switch]$NoModels,
    [switch]$NoAutoDeps,        # skip winget auto-install prompts
    [switch]$YesToAll,          # answer 'y' to every install prompt
    [switch]$Rebuild,           # force a clean llama.cpp recompile
    [switch]$Reinstall          # force pip reinstall into the venv
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # speeds up Invoke-WebRequest

$InstallDir = $PSScriptRoot
$ModelsDir  = Join-Path $InstallDir 'models'
$LlamaDir   = Join-Path $InstallDir 'llama.cpp'
$VenvDir    = Join-Path $InstallDir 'venv'
$Launcher   = Join-Path $InstallDir 'launcher.ps1'
$Log        = Join-Path $InstallDir 'install.log'

New-Item -ItemType Directory -Force -Path $ModelsDir | Out-Null
"=== Install started: $(Get-Date) ===" | Set-Content -Path $Log -Encoding utf8

function Log    { param($m) Write-Host "[+] $m" -ForegroundColor Green;  Add-Content $Log "[+] $m" }
function Warn   { param($m) Write-Host "[!] $m" -ForegroundColor Yellow; Add-Content $Log "[!] $m" }
function Info   { param($m) Write-Host "[.] $m" -ForegroundColor Cyan }
function Fail   { param($m) Write-Host "[x] $m" -ForegroundColor Red;    Add-Content $Log "[x] $m"; exit 1 }
function Br     { Write-Host "" }

# ASCII progress bar for the overall install - readable both on screen and in
# install.log (no Unicode block glyphs, so a plain text file stays legible).
$script:STEP_NUM   = 0
$script:STEP_TOTAL = 12
function Step {
    param([string]$Title)
    $script:STEP_NUM++
    $n = $script:STEP_NUM; $t = $script:STEP_TOTAL
    if ($n -gt $t) { $t = $n; $script:STEP_TOTAL = $n }
    $width  = 24
    $filled = [int][Math]::Floor(($n / $t) * $width)
    $pct    = [int][Math]::Floor(($n / $t) * 100)
    $bar    = ('#' * $filled) + ('-' * ($width - $filled))
    $line   = ("[{0}] {1,3}%  step {2}/{3}  {4}" -f $bar, $pct, $n, $t, $Title)
    Write-Host ""
    Write-Host $line -ForegroundColor White
    Add-Content $Log ""
    Add-Content $Log $line
}

function Invoke-Tool {
    # Run a native command, merge stderr into stdout, append combined output to
    # the log, and RETURN its exit code. Native tools (pip, git, cmake) routinely
    # print warnings/progress to stderr; under $ErrorActionPreference='Stop' a
    # 2>&1 capture turns that stderr into a terminating NativeCommandError and
    # aborts the script on a harmless notice. We relax to 'Continue' for the call
    # and judge success purely by the real exit code.
    param(
        [Parameter(Mandatory)][string]$Exe,
        [string[]]$Arguments = @(),
        [string]$LogFile
    )
    if (-not $LogFile) { $LogFile = $Log }
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $Exe @Arguments 2>&1 | Add-Content $LogFile
    } finally {
        $ErrorActionPreference = $prev
    }
    return $LASTEXITCODE
}

Clear-Host
Write-Host @'
+======================================================+
|   winc.cpp - AI LOCAL STACK INSTALLER (Windows)      |
|   llama.cpp + LiteLLM + Speculative Decoding         |
|   Curated for 16 GB NVIDIA GPUs                      |
+======================================================+
'@ -ForegroundColor White
Info "Install dir: $InstallDir"
Info "Log:         $Log"
Br

# -----------------------------------------------------------------------------
# Hardware detection
# -----------------------------------------------------------------------------
function Detect-Hardware {
    $script:GPU_VENDOR  = 'cpu'
    $script:GPU_NAME    = 'None (CPU only)'
    $script:VRAM_MB     = 0
    $script:TOTAL_RAM_GB = [int]([Math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1GB))

    $nvsmi = Get-Command nvidia-smi -ErrorAction SilentlyContinue
    if ($nvsmi) {
        try {
            $vram = (& nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>$null | Select-Object -First 1).Trim()
            $name = (& nvidia-smi --query-gpu=name        --format=csv,noheader        2>$null | Select-Object -First 1).Trim()
            if ($vram -match '^\d+$' -and [int]$vram -gt 0) {
                $script:GPU_VENDOR = 'nvidia'
                $script:GPU_NAME   = if ($name) { $name } else { 'NVIDIA GPU' }
                $script:VRAM_MB    = [int]$vram
                Log "NVIDIA: $GPU_NAME ($VRAM_MB MB) | System RAM: ${TOTAL_RAM_GB} GB"
                return
            }
        } catch {}
    }
    Warn "No NVIDIA GPU detected - falling back to CPU. System RAM: ${TOTAL_RAM_GB} GB"
}

function Pick-Context {
    # Claude Code system prompt is ~34K. Need >= 40960 to feel native.
    $ctx = 8192
    if ($GPU_VENDOR -eq 'nvidia') {
        if     ($VRAM_MB -ge 24000) { $ctx = 131072 }
        elseif ($VRAM_MB -ge 16000) { $ctx = 65536  }   # tuned default for 16 GB
        elseif ($VRAM_MB -ge 8000)  { $ctx = 49152  }
        elseif ($VRAM_MB -ge 6000)  { $ctx = 40960  }
        else                         { $ctx = 16384  }
    } else {
        if     ($TOTAL_RAM_GB -ge 32) { $ctx = 49152 }
        elseif ($TOTAL_RAM_GB -ge 16) { $ctx = 40960 }
        else                           { $ctx = 16384 }
    }
    $script:CTX = $ctx
    Log "Context: $CTX tokens"
}

# -----------------------------------------------------------------------------
# Auto-install helpers (winget) - saves you the manual install dance
# -----------------------------------------------------------------------------
function Add-KnownToolPaths {
    # winget often updates the *registry* PATH but the freshly-installed package's
    # bin dir may not land in this process. Probe the standard install locations
    # directly and prepend any that exist. This is what lets the script keep going
    # in the SAME run right after installing a tool - no "close and reopen" step.
    $cands = @(
        "$env:ProgramFiles\Git\cmd",
        "$env:ProgramFiles\CMake\bin",
        "${env:ProgramFiles(x86)}\CMake\bin",
        "$env:LOCALAPPDATA\Programs\Python\Python312",
        "$env:LOCALAPPDATA\Programs\Python\Python312\Scripts",
        "$env:LOCALAPPDATA\Programs\Python\Python311",
        "$env:LOCALAPPDATA\Programs\Python\Python311\Scripts",
        "$env:LOCALAPPDATA\Microsoft\WindowsApps"
    )
    # CUDA: newest versioned toolkit bin
    $cudaRoot = "$env:ProgramFiles\NVIDIA GPU Computing Toolkit\CUDA"
    if (Test-Path $cudaRoot) {
        Get-ChildItem $cudaRoot -Directory -ErrorAction SilentlyContinue |
            Sort-Object Name -Descending | Select-Object -First 1 |
            ForEach-Object { $cands += (Join-Path $_.FullName 'bin') }
    }
    foreach ($c in $cands) {
        if ((Test-Path $c) -and ($env:PATH -notlike "*$c*")) { $env:PATH = "$c;$env:PATH" }
    }
}

function Refresh-Path {
    # Re-read PATH from the registry (winget installs land there), then probe the
    # well-known install dirs so the new tools are usable without a shell restart.
    $m = [Environment]::GetEnvironmentVariable('PATH','Machine')
    $u = [Environment]::GetEnvironmentVariable('PATH','User')
    $env:PATH = "$m;$u"
    Add-KnownToolPaths
}

function Confirm-Install {
    param([string]$Prompt)
    if ($YesToAll)   { Log "  -> auto-yes ($Prompt)"; return $true }
    if ($NoAutoDeps) { return $false }
    $a = Read-Host "  Install $Prompt now via winget? [Y/n]"
    return ($a -eq '' -or $a -match '^[yY]')
}

function Winget-Install {
    param([string]$Id, [string]$Friendly, [string]$Override = '')
    Info "Installing $Friendly ($Id) via winget. This can take a few minutes..."
    Add-Content $Log "[.] winget install $Id"
    $wargs = @('install','--id',$Id,'--exact','--silent','--accept-source-agreements','--accept-package-agreements','--disable-interactivity')
    if ($Override) { $wargs += @('--override', $Override) }
    # Let winget draw its own progress directly to the console. Do NOT pipe its
    # output into install.log: winget emits VT/Unicode block-glyph progress that
    # turns into unreadable mojibake in a plain text file. We log a clean result.
    & winget @wargs
    $rc = $LASTEXITCODE
    if ($rc -ne 0 -and $rc -ne -1978335189) {  # -1978335189 = "already installed"
        Warn "winget exit $rc for $Friendly - continuing"
        return $false
    }
    Log "winget: $Friendly installed (or already present)"
    Refresh-Path
    return $true
}

function Find-VsInstall {
    $vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
    if (-not (Test-Path $vswhere)) { return $null }
    $p = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath 2>$null
    if ($p) { return $p.Trim() }
    return $null
}

function Enter-VcEnv {
    # Source vcvars64.bat so cl.exe / link.exe / Windows SDK are on PATH without
    # needing the "Developer PowerShell for VS 2022" shortcut.
    $vsPath = Find-VsInstall
    if (-not $vsPath) { return $false }
    $vcvars = Join-Path $vsPath 'VC\Auxiliary\Build\vcvars64.bat'
    if (-not (Test-Path $vcvars)) { return $false }
    Info "Loading MSVC environment from $vcvars ..."
    $lines = & cmd /c "`"$vcvars`" >nul 2>&1 && set"
    foreach ($line in $lines) {
        if ($line -match '^([^=]+)=(.*)$') {
            [Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process')
        }
    }
    return ($null -ne (Get-Command cl.exe -ErrorAction SilentlyContinue))
}

# -----------------------------------------------------------------------------
# Dependency check - offers winget install for anything missing, then reloads PATH
# -----------------------------------------------------------------------------
function Check-Deps {
    Log "Checking dependencies..."
    Refresh-Path   # pick up anything installed in a prior partial run
    $haveWinget = [bool](Get-Command winget -ErrorAction SilentlyContinue)
    if (-not $haveWinget) {
        Warn "winget not found. Auto-install disabled - install App Installer from the Microsoft Store."
    }

    # Map: command-on-PATH -> winget id + friendly name + override args
    $coreDeps = @(
        @{ Cmd='git';    Id='Git.Git';            Name='Git' },
        @{ Cmd='cmake';  Id='Kitware.CMake';      Name='CMake' },
        @{ Cmd='python'; Id='Python.Python.3.12'; Name='Python 3.12' }
    )

    foreach ($dep in $coreDeps) {
        if (Get-Command $dep.Cmd -ErrorAction SilentlyContinue) { continue }
        Warn "Missing: $($dep.Name) ($($dep.Cmd) not on PATH)"
        if ($haveWinget -and (Confirm-Install $dep.Name)) {
            [void](Winget-Install -Id $dep.Id -Friendly $dep.Name)
        }
    }

    # MSVC C++ build tools - required for compiling llama.cpp
    if (-not (Find-VsInstall)) {
        Warn "Missing: MSVC C++ build tools (Visual Studio 2022 BuildTools + VC workload)"
        if ($haveWinget -and (Confirm-Install 'Visual Studio 2022 Build Tools (with C++ workload)')) {
            [void](Winget-Install -Id 'Microsoft.VisualStudio.2022.BuildTools' `
                -Friendly 'VS 2022 BuildTools (VC workload)' `
                -Override '--quiet --wait --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended')
        }
    } else {
        Log "MSVC: $(Find-VsInstall)"
    }

    # CUDA Toolkit - only if user has an NVIDIA GPU
    if ($GPU_VENDOR -eq 'nvidia') {
        if (-not (Get-Command nvcc -ErrorAction SilentlyContinue)) {
            Warn "Missing: CUDA Toolkit (nvcc). Without it, llama.cpp builds CPU-only."
            if ($haveWinget -and (Confirm-Install 'CUDA Toolkit (large download, ~3 GB)')) {
                [void](Winget-Install -Id 'Nvidia.CUDA' -Friendly 'CUDA Toolkit')
            }
        }
    }

    Refresh-Path

    # Re-probe after any installs
    $stillMissing = @()
    foreach ($dep in $coreDeps) {
        if (-not (Get-Command $dep.Cmd -ErrorAction SilentlyContinue)) { $stillMissing += $dep.Cmd }
    }
    if ($stillMissing.Count -gt 0) {
        Warn "After install, still missing on PATH: $($stillMissing -join ', ')"
        Warn "winget sometimes won't update PATH in the current process."
        Fail "Close this window, open a NEW PowerShell, and re-run install.cmd / install.ps1."
    }

    if (Get-Command nvcc -ErrorAction SilentlyContinue) {
        $nvccVer = (& nvcc --version | Select-String 'release' | Out-String).Trim()
        Log "CUDA: $nvccVer"
    } elseif ($GPU_VENDOR -eq 'nvidia') {
        Warn "Continuing without CUDA - llama.cpp will build CPU-only."
    }

    $pv = & python -c "import sys; print(sys.version_info.minor)" 2>$null
    if (-not $pv -or [int]$pv -lt 9) { Fail "Python 3.9+ required (found minor=$pv)" }
    Log "Dependencies OK"
}

# -----------------------------------------------------------------------------
# Python venv + LiteLLM + HuggingFace CLI
# -----------------------------------------------------------------------------
function Resolve-HfCli {
    # Prefer the modern 'hf' CLI. The old 'huggingface-cli' is deprecated in
    # huggingface_hub 1.x and now refuses to run ("use hf instead"), so it must
    # NOT be chosen even when its shim still exists in Scripts.
    $hf    = Join-Path $VenvDir 'Scripts\hf.exe'
    $hfOld = Join-Path $VenvDir 'Scripts\huggingface-cli.exe'
    if     (Test-Path $hf)    { return $hf }
    elseif (Test-Path $hfOld) { return $hfOld }
    return $null
}

# litellm pulls native-wheel deps (orjson, tokenizers, etc.). Those ship
# prebuilt wheels only for "mature" Python versions; a too-new Python (e.g.
# 3.14) forces a from-source Rust build that fails. Require 3.9-3.13.
function Test-PyMinorOk { param($v) return ($v -match '^3\.(9|1[0-3])$') }

function Get-PyVersion {
    param([string]$Exe, [string[]]$Pre = @())
    try {
        # Capture to a variable and snapshot $LASTEXITCODE BEFORE any Select-Object.
        # Piping a native command straight into 'Select-Object -First 1' stops the
        # pipeline early, which clobbers $LASTEXITCODE to a non-zero value and made
        # this wrongly reject a perfectly good interpreter.
        $out  = & $Exe @Pre -c "import sys;print('%d.%d'%sys.version_info[:2])" 2>$null
        $code = $LASTEXITCODE
        $v    = ($out | Select-Object -First 1)
        if ($code -eq 0 -and $v) { return ([string]$v).Trim() }
    } catch {}
    return $null
}

function Find-CompatiblePython {
    # Returns @{ Exe=..; Pre=@(..); Ver=.. } for a Python in 3.9-3.13, else $null.
    # Prefer 3.12 (widest wheel coverage), then 3.13/3.11/3.10/3.9.
    $cands = @(
        @{ Exe='py';      Pre=@('-3.12') },
        @{ Exe='py';      Pre=@('-3.13') },
        @{ Exe='py';      Pre=@('-3.11') },
        @{ Exe='py';      Pre=@('-3.10') },
        @{ Exe='py';      Pre=@('-3.9')  },
        @{ Exe='python';  Pre=@()        },
        @{ Exe='python3'; Pre=@()        }
    )
    foreach ($c in $cands) {
        if (-not (Get-Command $c.Exe -ErrorAction SilentlyContinue)) { continue }
        $v = Get-PyVersion $c.Exe $c.Pre
        if ($v -and (Test-PyMinorOk $v)) {
            Log "Compatible Python: $($c.Exe) $($c.Pre -join ' ') -> $v"
            return @{ Exe=$c.Exe; Pre=$c.Pre; Ver=$v }
        }
    }
    return $null
}

function Setup-Venv {
    Log "Setting up venv..."
    $py = Join-Path $VenvDir 'Scripts\python.exe'

    # If an existing venv was built with an incompatible Python (e.g. a prior run
    # under 3.14), discard it so we can recreate with a supported interpreter.
    if (Test-Path $py) {
        $existingVer = Get-PyVersion $py
        if ($existingVer -and -not (Test-PyMinorOk $existingVer)) {
            Warn "Existing venv uses Python $existingVer (need 3.9-3.13). Recreating..."
            Remove-Item $VenvDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    if (-not (Test-Path $py)) {
        $pyCmd = Find-CompatiblePython
        if (-not $pyCmd) {
            $def = Get-PyVersion 'python'
            Warn "No compatible Python found (need 3.9-3.13; your default is ${def})."
            if ((Get-Command winget -ErrorAction SilentlyContinue) -and `
                (Confirm-Install 'Python 3.12 (your Python is too new for prebuilt wheels)')) {
                [void](Winget-Install -Id 'Python.Python.3.12' -Friendly 'Python 3.12')
                $pyCmd = Find-CompatiblePython
            }
        }
        if (-not $pyCmd) {
            Fail "Need Python 3.9-3.13 for the venv. Install Python 3.12 (winget install Python.Python.3.12) and re-run."
        }
        Log "Creating venv with $($pyCmd.Exe) $($pyCmd.Pre -join ' ') (Python $($pyCmd.Ver))..."
        [void](Invoke-Tool $pyCmd.Exe ($pyCmd.Pre + @('-m','venv',$VenvDir)))
    }
    if (-not (Test-Path $py)) { Fail "venv python not found: $py" }

    # Idempotency: if litellm + HF CLI are already in this venv, don't re-pip.
    $hf = Resolve-HfCli
    $liteOk = $false
    if ($hf) {
        & $py -c "import litellm" 2>$null
        $liteOk = ($LASTEXITCODE -eq 0)
    }
    if ($hf -and $liteOk -and -not $Reinstall) {
        $script:HF_CLI = $hf
        Log "Venv already provisioned (litellm + HF CLI present). Skipping pip. Use -Reinstall to force."
        return
    }

    Info "Installing Python packages into venv (litellm, huggingface_hub)..."
    $pipLog = Join-Path $InstallDir 'pip.log'
    "=== pip run: $(Get-Date) | python: $py ===" | Set-Content $pipLog

    # Capture pip output into a variable then Add-Content it. We deliberately
    # avoid the *>> / > redirection operators: under Windows PowerShell 5.1 those
    # write UTF-16LE, which renders as spaced-out garbage in a UTF-8/ANSI log.
    # Relax ErrorActionPreference so a pip stderr WARNING (e.g. an unknown extra)
    # doesn't become a terminating NativeCommandError and kill the install.
    # NOTE: huggingface_hub ships its CLI (huggingface-cli / hf) in the BASE
    # package now - the old "[cli]" extra was removed in hub 1.x and errors.
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        # Upgrade pip + build backends first so any source-only deps can build.
        $out1 = & $py -m pip install --upgrade pip setuptools wheel --disable-pip-version-check --no-input 2>&1
        $out1 | Add-Content $pipLog
        $out2 = & $py -m pip install "litellm[proxy]" huggingface_hub requests --disable-pip-version-check --no-input 2>&1
        $pipRc = $LASTEXITCODE
        $out2 | Add-Content $pipLog
    } finally {
        $ErrorActionPreference = $prevEAP
    }
    if ($pipRc -ne 0) {
        Warn "pip install failed (exit $pipRc). Last 25 lines of output:"
        $out2 | Select-Object -Last 25 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
        Fail "pip install failed - full log: $pipLog"
    }

    $script:HF_CLI = Resolve-HfCli
    if (-not $HF_CLI) { Fail "HuggingFace CLI not found after install (see $pipLog)" }
    Log "Venv ready | HF CLI: $HF_CLI"
}

# -----------------------------------------------------------------------------
# Build llama.cpp (MSVC + CUDA when available)
# -----------------------------------------------------------------------------
function Find-LlamaBins {
    param($bd)
    $script:LLAMA_SERVER_BIN = @(
        (Join-Path $bd 'bin\Release\llama-server.exe'),
        (Join-Path $bd 'bin\llama-server.exe'),
        (Join-Path $bd 'Release\llama-server.exe')
    ) | Where-Object { Test-Path $_ } | Select-Object -First 1
    $script:LLAMA_CLI_BIN = @(
        (Join-Path $bd 'bin\Release\llama-cli.exe'),
        (Join-Path $bd 'bin\llama-cli.exe'),
        (Join-Path $bd 'Release\llama-cli.exe')
    ) | Where-Object { Test-Path $_ } | Select-Object -First 1
}

function Build-Llama {
    $bd = Join-Path $LlamaDir 'build'

    # Idempotency: reuse an existing compiled binary unless told to rebuild.
    Find-LlamaBins $bd
    if ($LLAMA_SERVER_BIN -and -not $Rebuild -and -not $NoBuild) {
        Log "Reusing existing llama-server: $LLAMA_SERVER_BIN (use -Rebuild to recompile)"
        if ($LLAMA_CLI_BIN) { Log "Reusing llama-cli: $LLAMA_CLI_BIN" }
        return
    }
    if ($NoBuild) {
        if ($LLAMA_SERVER_BIN) { Warn "Skipping build (-NoBuild); using existing $LLAMA_SERVER_BIN"; return }
        Fail "-NoBuild set but no existing llama-server.exe found. Remove -NoBuild to compile."
    }

    # Make cl.exe / link.exe / Windows SDK visible to cmake without requiring
    # the "Developer PowerShell for VS 2022" shortcut.
    if (-not (Get-Command cl.exe -ErrorAction SilentlyContinue)) {
        if (-not (Enter-VcEnv)) {
            Fail "MSVC environment unavailable. Install VS Build Tools (VC workload) and re-run."
        }
        Log "MSVC env loaded (cl.exe on PATH)."
    }

    Log "Setting up llama.cpp..."
    if (-not (Test-Path (Join-Path $LlamaDir '.git'))) {
        if ((Invoke-Tool 'git' @('clone','--depth=1','https://github.com/ggerganov/llama.cpp',$LlamaDir)) -ne 0) {
            Fail "git clone failed - see $Log"
        }
    } else {
        [void](Invoke-Tool 'git' @('-C',$LlamaDir,'pull','--ff-only'))
    }

    New-Item -ItemType Directory -Force -Path $bd | Out-Null

    $cfg = @('-DCMAKE_BUILD_TYPE=Release')
    if ($GPU_VENDOR -eq 'nvidia' -and (Get-Command nvcc -ErrorAction SilentlyContinue)) {
        $cfg += '-DGGML_CUDA=ON'
    } else {
        $cfg += '-DGGML_AVX2=ON'
    }

    $nc = [Environment]::ProcessorCount

    Log "cmake configure..."
    if ((Invoke-Tool 'cmake' (@('-S',$LlamaDir,'-B',$bd) + $cfg)) -ne 0) {
        Fail "cmake configure failed - see $Log"
    }

    Log "Building with $nc threads (this takes a while)..."
    if ((Invoke-Tool 'cmake' @('--build',$bd,'--config','Release','-j',"$nc")) -ne 0) {
        Fail "build failed - see $Log"
    }

    Find-LlamaBins $bd
    if (-not $LLAMA_SERVER_BIN) { Fail "llama-server.exe not found after build" }
    Log "llama-server: $LLAMA_SERVER_BIN"
    if ($LLAMA_CLI_BIN) { Log "llama-cli: $LLAMA_CLI_BIN" } else { Warn "llama-cli not found" }
}

# -----------------------------------------------------------------------------
# Probe llama-server feature flags (so the launcher only passes supported ones)
# -----------------------------------------------------------------------------
function Detect-Flags {
    $script:FLASH_ATTN_SUPPORT = $false
    $script:FLASH_ATTN_NEEDS_VALUE = $false
    $script:MLOCK_SUPPORT = $false
    $script:PRIO_SUPPORT  = $false
    $script:SPEC_SUPPORT  = $false
    if (-not $LLAMA_SERVER_BIN) { return }
    $h = & $LLAMA_SERVER_BIN --help 2>&1 | Out-String
    if ($h -match '--flash-attn') {
        $script:FLASH_ATTN_SUPPORT = $true
        if ($h -match '--flash-attn[^\r\n]*(on|off|auto)') { $script:FLASH_ATTN_NEEDS_VALUE = $true }
    }
    if ($h -match '--mlock')             { $script:MLOCK_SUPPORT = $true }
    if ($h -match '--prio')              { $script:PRIO_SUPPORT  = $true }
    if ($h -match '--spec-draft-model')  { $script:SPEC_SUPPORT  = $true }
    Log "flash=$FLASH_ATTN_SUPPORT | mlock=$MLOCK_SUPPORT | prio=$PRIO_SUPPORT | spec=$SPEC_SUPPORT"
}

# -----------------------------------------------------------------------------
# Configure an ISOLATED Claude Code config dir for the local-model instance.
# We deliberately do NOT touch the user's global ~/.claude. The launcher points
# Claude Code at this dir via CLAUDE_CONFIG_DIR, so the local instance has its
# own settings, credentials, history and todos - it cannot collide with a
# logged-in Opus instance running in another terminal.
# -----------------------------------------------------------------------------
function Configure-ClaudeSettings {
    $d = Join-Path $InstallDir '.claude-local'
    $f = Join-Path $d 'settings.json'
    New-Item -ItemType Directory -Force -Path $d | Out-Null
    @'
{
  "env": {
    "CLAUDE_CODE_ENABLE_TELEMETRY": "0",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "CLAUDE_CODE_ATTRIBUTION_HEADER": "0"
  },
  "attribution": { "commit": "", "pr": "" },
  "hasCompletedOnboarding": true
}
'@ | Set-Content -Path $f -Encoding utf8
    Log "Isolated config written: $f (ATTRIBUTION_HEADER=0 fixes 90% KV penalty)"
    Log "Global ~/.claude is left untouched - safe to run alongside your Opus session."
}

# -----------------------------------------------------------------------------
# HF token (optional)
# -----------------------------------------------------------------------------
function Ask-HfToken {
    Br
    Write-Host "Hugging Face Token (optional - press Enter to skip)" -ForegroundColor White
    $script:HF_TOKEN = Read-Host "HF Token"
    if ($HF_TOKEN) { Log "HF token set" } else { Warn "No HF token (gated models like Llama/Gemma may fail)" }
}

# -----------------------------------------------------------------------------
# Model menu - loaded from the shared catalog.ps1 (also used by the winc CLI),
# curated for 16 GB NVIDIA GPUs. Quants fit weights + KV cache at 65K context.
# -----------------------------------------------------------------------------
. (Join-Path $PSScriptRoot 'catalog.ps1')
$MODELS = $WINC_MODELS

function Show-ModelMenu {
    Br
    Write-Host "==========================================================" -ForegroundColor White
    Write-Host "  MODEL SELECTION  -  GPU: $GPU_NAME | VRAM: ${VRAM_MB} MB" -ForegroundColor White
    Write-Host "==========================================================" -ForegroundColor White
    Br
    $rec = if     ($VRAM_MB -ge 16000) { '1' }
           elseif ($VRAM_MB -ge 12000) { '3' }
           elseif ($VRAM_MB -ge 10000) { '5' }
           elseif ($VRAM_MB -ge 6000)  { '9' }
           else                         { '9' }
    foreach ($m in $MODELS) {
        $tag = ''
        if ($m.N.ToString() -eq $rec) { $tag = '  <- RECOMMENDED' }
        Write-Host ("  [{0}] {1,-50}  (~{2})  {3}" -f $m.N, $m.Name, $m.Size, $tag) -ForegroundColor Cyan
        Write-Host ("        {0}" -f $m.Note)
    }
    Write-Host "  [C] Custom - HuggingFace repo + filename" -ForegroundColor Yellow
    Br
    Write-Host "==========================================================" -ForegroundColor White
    Br
    $script:MODEL_CHOICE = Read-Host "Choice(s) [1-9 or C, comma-sep, default=$rec]"
    if (-not $MODEL_CHOICE) { $script:MODEL_CHOICE = $rec }
}

# Speculative-decode draft model lookup. Qwen3 family pairs with Qwen3-0.6B.
function Get-DraftPair {
    param($n)
    switch ($n) {
        '5' { return @{ Repo='bartowski/Qwen_Qwen3-0.6B-GGUF'; File='Qwen_Qwen3-0.6B-Q8_0.gguf' } }   # Qwen3-14B + Qwen3-0.6B draft
        default { return $null }   # other families have no matched/verified draft pair
    }
}

function Download-Model {
    param($repo, $file, $size)
    Br
    Write-Host "  Downloading: $file (~$size)" -ForegroundColor Cyan
    Write-Host "  From:        $repo"           -ForegroundColor Cyan
    Br
    # Download via the venv python + hf_get.py (clean 1s progress bar + ETA),
    # NOT the hf.exe shim (which hardcodes the venv path and breaks on rename).
    # hf_get.py reads the token from the HF_TOKEN env var automatically.
    if ($HF_TOKEN) { $env:HF_TOKEN = $HF_TOKEN }
    $py = Join-Path $VenvDir 'Scripts\python.exe'
    & $py (Join-Path $InstallDir 'hf_get.py') $repo $file $ModelsDir
    return ($LASTEXITCODE -eq 0)
}

function Download-Models {
    if ($NoModels) { Warn "Skipping model downloads (-NoModels)"; return }
    foreach ($raw in ($MODEL_CHOICE -split ',')) {
        $c = $raw.Trim()
        if ($c -ieq 'C') {
            $cr = Read-Host "  HF repo"
            $cf = Read-Host "  Filename"
            if ($cr -and $cf) { [void](Download-Model -repo $cr -file $cf -size '?') }
            continue
        }
        $m = $MODELS | Where-Object { $_.N.ToString() -eq $c } | Select-Object -First 1
        if (-not $m) { Warn "Invalid choice '$c'"; continue }
        $target = Join-Path $ModelsDir $m.File
        if (Test-Path $target) { Log "Exists: $($m.File)"; continue }
        Log "Downloading $($m.File) (~$($m.Size))..."
        if (Download-Model -repo $m.Repo -file $m.File -size $m.Size) {
            if (Test-Path $target) { Log "Done: $($m.File)" } else { Warn "Failed: $($m.File)" }
        } else { Warn "Failed: $($m.File)" }
    }
}

# -----------------------------------------------------------------------------
# Speculative decoding offer (needs >=24 GB VRAM to be worth it)
# -----------------------------------------------------------------------------
function Ask-Speculative {
    $script:SPEC_ENABLED    = $false
    $script:DRAFT_FILE_NAME = ''
    $first = ($MODEL_CHOICE -split ',')[0].Trim()
    $pair  = Get-DraftPair $first
    if (-not $pair) { Info "No draft model available for this model family."; return }
    if (-not $SPEC_SUPPORT) { Info "Your llama.cpp build doesn't support speculative decoding."; return }
    if ($VRAM_MB -lt 24000) {
        Info "Speculative decoding skipped: needs >=24 GB VRAM (you have ${VRAM_MB} MB)."
        Info "Draft model + extra KV cache would leave too little headroom on 16 GB."
        return
    }
    Br
    Write-Host "==========================================================" -ForegroundColor White
    Write-Host "  SPECULATIVE DECODING  (optional - up to 2.5x faster)"     -ForegroundColor White
    Write-Host "==========================================================" -ForegroundColor White
    Br
    Write-Host "  Uses a 0.5B draft model to predict tokens in parallel."
    Write-Host "  Main model verifies them - NO quality loss."
    Write-Host "  Extra VRAM: ~400 MB | Draft: $($pair.File)"
    Br
    $ans = Read-Host "Enable speculative decoding? [y/N]"
    if ($ans -match '^[yY]') {
        $script:SPEC_ENABLED    = $true
        $script:DRAFT_FILE_NAME = $pair.File
        $tgt = Join-Path $ModelsDir $pair.File
        if (-not (Test-Path $tgt)) {
            Log "Downloading draft model..."
            [void](Download-Model -repo $pair.Repo -file $pair.File -size '~400 MB')
        } else { Log "Draft exists: $($pair.File)" }
    }
}

# -----------------------------------------------------------------------------
# Generate launcher.ps1 (baked with the values detected above)
# -----------------------------------------------------------------------------
function Write-Launcher {
    Log "Writing launcher: $Launcher"

    $header = @"
# launcher.ps1 - generated by install.ps1
# Baked: $(Get-Date -Format o)
`$ErrorActionPreference = 'Stop'

# Paths are derived from this script's own location, so the whole winc.cpp folder
# can be moved or renamed and the launcher still finds everything (no baked paths).
`$INSTALL_DIR     = `$PSScriptRoot
`$MODELS_DIR      = Join-Path `$INSTALL_DIR 'models'
`$VENV_DIR        = Join-Path `$INSTALL_DIR 'venv'
`$GPU_VENDOR      = '$GPU_VENDOR'
`$GPU_NAME        = '$($GPU_NAME -replace "'","''")'
`$TOTAL_RAM_GB    = $TOTAL_RAM_GB
`$DEFAULT_CTX     = $CTX
`$FLASH_ATTN_SUPPORT     = `$$FLASH_ATTN_SUPPORT
`$FLASH_ATTN_NEEDS_VALUE = `$$FLASH_ATTN_NEEDS_VALUE
`$MLOCK_SUPPORT   = `$$MLOCK_SUPPORT
`$PRIO_SUPPORT    = `$$PRIO_SUPPORT
`$SPEC_ENABLED    = `$$SPEC_ENABLED
`$DRAFT_FILE_NAME = '$DRAFT_FILE_NAME'
`$LLAMA_PORT      = 8080
`$LLM_PROXY_PORT  = 4000
"@

    $body = @'

# -- cleanup -----------------------------------------------------------------
$script:LLAMA_PROC   = $null
$script:LITELLM_PROC = $null

function Stop-Children {
    Write-Host ""
    Write-Host "[.] Shutting down..." -ForegroundColor Cyan
    foreach ($p in @($script:LLAMA_PROC, $script:LITELLM_PROC)) {
        if ($p -and -not $p.HasExited) {
            try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch {}
        }
    }
    # belt-and-braces sweep
    Get-CimInstance Win32_Process -Filter "Name='llama-server.exe'" -ErrorAction SilentlyContinue |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Get-CimInstance Win32_Process -Filter "Name='python.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -match 'litellm.*--port\s+' + $LLM_PROXY_PORT } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Remove-Item -Force "$env:TEMP\litellm_runtime.yaml" -ErrorAction SilentlyContinue
    Write-Host "[+] Stopped." -ForegroundColor Green
}
Register-EngineEvent -SourceIdentifier PowerShell.Exiting -SupportEvent -Action { Stop-Children } | Out-Null
[Console]::TreatControlCAsInput = $false

# -- pre-flight: kill stale instances on our ports ---------------------------
Get-CimInstance Win32_Process -Filter "Name='llama-server.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match "--port\s+$LLAMA_PORT" } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Get-CimInstance Win32_Process -Filter "Name='python.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match "litellm.*--port\s+$LLM_PROXY_PORT" } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Start-Sleep -Milliseconds 500

# -- venv activate (we just need $env:PATH and python on PATH) ---------------
$venvScripts = Join-Path $VENV_DIR 'Scripts'
$env:PATH    = "$venvScripts;$env:PATH"
$venvPython  = Join-Path $venvScripts 'python.exe'

# -- locate llama.cpp binaries relative to this folder (no baked paths) -------
$llamaBuild = Join-Path $INSTALL_DIR 'llama.cpp\build'
$LLAMA_SERVER = @(
    (Join-Path $llamaBuild 'bin\Release\llama-server.exe'),
    (Join-Path $llamaBuild 'bin\llama-server.exe'),
    (Join-Path $llamaBuild 'Release\llama-server.exe')
) | Where-Object { Test-Path $_ } | Select-Object -First 1
$LLAMA_CLI = @(
    (Join-Path $llamaBuild 'bin\Release\llama-cli.exe'),
    (Join-Path $llamaBuild 'bin\llama-cli.exe'),
    (Join-Path $llamaBuild 'Release\llama-cli.exe')
) | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $LLAMA_SERVER) {
    Write-Host "[x] llama-server.exe not found under $llamaBuild - run install.cmd." -ForegroundColor Red
    exit 1
}

$totalCores = [Environment]::ProcessorCount
$threads    = [Math]::Max(2, $totalCores - 4)

Clear-Host
Write-Host "========================================================" -ForegroundColor White
Write-Host "  AI LOCAL LAUNCHER (Windows)" -ForegroundColor White
Write-Host "  GPU: $GPU_NAME | Cores: $totalCores | Threads: $threads" -ForegroundColor White
Write-Host "  RAM: ${TOTAL_RAM_GB} GB" -ForegroundColor White
Write-Host "========================================================" -ForegroundColor White
Write-Host ""

# -- model selection --------------------------------------------------------
$models = Get-ChildItem -Path $MODELS_DIR -Filter '*.gguf' -File |
          Where-Object { $_.Name -notmatch '0\.5B' } |
          Sort-Object Name
if ($models.Count -eq 0) { Write-Host "[x] No models found in $MODELS_DIR" -ForegroundColor Red; exit 1 }

if ($env:WINC_MODEL) {
    # Non-interactive: model chosen by the winc CLI (filename / basename / substring).
    $sel = $models | Where-Object { $_.Name -eq $env:WINC_MODEL -or $_.BaseName -eq $env:WINC_MODEL -or $_.Name -like "*$($env:WINC_MODEL)*" } | Select-Object -First 1
    if (-not $sel) { Write-Host "[x] Model '$($env:WINC_MODEL)' not found in $MODELS_DIR" -ForegroundColor Red; exit 1 }
    $model = $sel.FullName; $modelName = $sel.BaseName
    Write-Host ("Model: {0}  (selected by winc)" -f $sel.Name)
} else {
    Write-Host "Available models:"
    for ($i = 0; $i -lt $models.Count; $i++) {
        $sizeGB = [Math]::Round($models[$i].Length / 1GB, 1)
        Write-Host ("  [{0}] {1}  ({2} GB)" -f $i, $models[$i].Name, $sizeGB)
    }
    Write-Host ""
    $midxRaw = Read-Host "Select model [0]"
    $midx    = if ($midxRaw -match '^\d+$') { [int]$midxRaw } else { 0 }
    if ($midx -lt 0 -or $midx -ge $models.Count) { Write-Host "[x] Invalid" -ForegroundColor Red; exit 1 }
    $model     = $models[$midx].FullName
    $modelName = $models[$midx].BaseName
    Write-Host ("Model: {0}" -f $models[$midx].Name)
}

$draftModel = ''
if ($SPEC_ENABLED -and $DRAFT_FILE_NAME) {
    $candidate = Join-Path $MODELS_DIR $DRAFT_FILE_NAME
    if (Test-Path $candidate) {
        $draftModel = $candidate
        Write-Host "Draft: $DRAFT_FILE_NAME (speculative decoding ON)"
    } else {
        Write-Host "[!] Draft not found - spec decode disabled" -ForegroundColor Yellow
        $SPEC_ENABLED = $false
    }
}
Write-Host ""

if ($env:WINC_MODE) {
    # Non-interactive: mode chosen by the winc CLI (1=cli, 2=claude, 3=opencode).
    $mode = $env:WINC_MODE
} else {
    if ($LLAMA_CLI -and (Test-Path $LLAMA_CLI)) { Write-Host "  [1] llama.cpp CLI     (direct chat)" }
    Write-Host "  [2] Claude Code       (via LiteLLM proxy)"
    Write-Host "  [3] OpenCode          (via LiteLLM proxy)"
    Write-Host ""
    $mode = Read-Host "Mode [2]"
    if (-not $mode) { $mode = '2' }
}
Write-Host ""

if ($mode -eq '1' -and (-not $LLAMA_CLI -or -not (Test-Path $LLAMA_CLI))) {
    Write-Host "[x] No llama-cli built" -ForegroundColor Red; exit 1
}

# -- runtime VRAM + context -------------------------------------------------
$vramMB = 0
if ($GPU_VENDOR -eq 'nvidia' -and (Get-Command nvidia-smi -ErrorAction SilentlyContinue)) {
    try {
        $r = (& nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>$null | Select-Object -First 1).Trim()
        if ($r -match '^\d+$') { $vramMB = [int]$r }
    } catch {}
}
if     ($vramMB -ge 24000) { $ctx = 131072 }
elseif ($vramMB -ge 16000) { $ctx = 65536  }
elseif ($vramMB -ge 8000)  { $ctx = 49152  }
elseif ($vramMB -ge 6000)  { $ctx = 40960  }
else                        { $ctx = $DEFAULT_CTX }

if ($mode -eq '2' -and $ctx -lt 40960) {
    Write-Host "[!] WARNING: Context $ctx may be too small for Claude Code" -ForegroundColor Yellow
}
Write-Host "[+] VRAM: $vramMB MB | Context: $ctx | Threads: $threads/$totalCores"

# -- runtime flags ----------------------------------------------------------
$extra = New-Object System.Collections.Generic.List[string]
if ($FLASH_ATTN_SUPPORT) {
    if ($FLASH_ATTN_NEEDS_VALUE) { $extra.Add('--flash-attn'); $extra.Add('on') }
    else { $extra.Add('--flash-attn') }
}
# mlock only on >=32 GB RAM machines (16 GB freezes under pressure)
if ($MLOCK_SUPPORT -and $TOTAL_RAM_GB -ge 32) { $extra.Add('--mlock') }
if ($PRIO_SUPPORT) { $extra.Add('--prio'); $extra.Add('2') }

if ($GPU_VENDOR -eq 'cpu') { $batch = 512;  $ubatch = 512;  $ngl = 0  }
else                        { $batch = 2048; $ubatch = 2048; $ngl = 99 }

$specFlags = @()
if ($SPEC_ENABLED -and $draftModel) {
    $specFlags = @('--spec-draft-model', $draftModel, '--spec-draft-ngl', "$ngl",
                   '--spec-draft-n-max', '16', '--spec-draft-n-min', '5')
    Write-Host "[+] Speculative decoding: ON (max=16, min=5)" -ForegroundColor Green
}

# -- CLI MODE ---------------------------------------------------------------
if ($mode -eq '1') {
    Write-Host "[+] CLI: -ngl $ngl -c $ctx -t $threads -b $batch $($specFlags -join ' ') $($extra -join ' ')" -ForegroundColor Green
    & $LLAMA_CLI -m $model -ngl $ngl -c $ctx -t $threads -b $batch -ub $ubatch `
        --cache-type-k q8_0 --cache-type-v q8_0 -cnv @specFlags @extra
    exit $LASTEXITCODE
}

# -- SERVER MODE ------------------------------------------------------------
$llamaLog   = Join-Path $env:TEMP 'llama.log'
$litellmLog = Join-Path $env:TEMP 'litellm.log'

$serverArgs = @('-m', $model, '--host', '127.0.0.1', '--port', "$LLAMA_PORT",
                '-ngl', "$ngl", '-c', "$ctx", '-t', "$threads",
                '-b', "$batch", '-ub', "$ubatch",
                '--cache-type-k', 'q8_0', '--cache-type-v', 'q8_0', '--metrics') + $specFlags + $extra

Write-Host "[+] Starting llama-server..." -ForegroundColor Green
$script:LLAMA_PROC = Start-Process -FilePath $LLAMA_SERVER -ArgumentList $serverArgs `
    -RedirectStandardOutput $llamaLog -RedirectStandardError "$llamaLog.err" `
    -NoNewWindow -PassThru

Write-Host "[+] Waiting for llama.cpp (up to 90s)..." -ForegroundColor Green
$ready = $false
for ($i = 1; $i -le 90; $i++) {
    try {
        $null = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$LLAMA_PORT/v1/models" -TimeoutSec 2
        Write-Host "[+] Ready (${i}s)." -ForegroundColor Green
        $ready = $true; break
    } catch { Start-Sleep -Seconds 1 }
}
if (-not $ready) {
    Write-Host "[!] llama-server failed - tail of log:" -ForegroundColor Red
    if (Test-Path $llamaLog) { Get-Content $llamaLog -Tail 20 }
    Stop-Children; exit 1
}

try {
    $modelsResp = Invoke-RestMethod -Uri "http://127.0.0.1:$LLAMA_PORT/v1/models"
    $mid = $modelsResp.data[0].id
} catch { $mid = $modelName }

# -- LiteLLM proxy config ---------------------------------------------------
$yaml = @"
model_list:
  - model_name: claude-sonnet-4-6
    litellm_params:
      model: openai/$mid
      api_base: http://127.0.0.1:$LLAMA_PORT/v1
      api_key: dummy
  - model_name: claude-sonnet-4-5
    litellm_params:
      model: openai/$mid
      api_base: http://127.0.0.1:$LLAMA_PORT/v1
      api_key: dummy
  - model_name: claude-haiku-4-5
    litellm_params:
      model: openai/$mid
      api_base: http://127.0.0.1:$LLAMA_PORT/v1
      api_key: dummy
litellm_settings:
  drop_params: true
  set_verbose: false
"@
$yamlPath = Join-Path $env:TEMP 'litellm_runtime.yaml'
$yaml | Set-Content -Path $yamlPath -Encoding utf8

Write-Host "[+] Starting LiteLLM..." -ForegroundColor Green
# Force UTF-8 so LiteLLM's startup banner doesn't crash under cp1252 when stdout
# is redirected to a log file (UnicodeEncodeError -> "Application startup failed").
$env:PYTHONUTF8       = '1'
$env:PYTHONIOENCODING = 'utf-8'
# Start via litellm_run.py (loads litellm's console entry point) NOT `-m litellm`
# (litellm has no __main__) and NOT litellm.exe (shim breaks if the folder moves).
$litellmRun  = Join-Path $INSTALL_DIR 'litellm_run.py'
$litellmArgs = @($litellmRun, '--config', $yamlPath, '--port', "$LLM_PROXY_PORT",
                 '--host', '127.0.0.1', '--telemetry', 'False')
$script:LITELLM_PROC = Start-Process -FilePath $venvPython -ArgumentList $litellmArgs `
    -RedirectStandardOutput $litellmLog -RedirectStandardError "$litellmLog.err" `
    -NoNewWindow -PassThru

$proxyReady = $false
for ($i = 1; $i -le 30; $i++) {
    try {
        $null = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$LLM_PROXY_PORT/health" -TimeoutSec 2
        Write-Host "[+] LiteLLM ready (${i}s)." -ForegroundColor Green
        $proxyReady = $true; break
    } catch { Start-Sleep -Seconds 1 }
}
if (-not $proxyReady) {
    Write-Host "[!] LiteLLM failed - tail of logs:" -ForegroundColor Red
    foreach ($lf in @($litellmLog, "$litellmLog.err")) {
        if ((Test-Path $lf) -and (Get-Item $lf).Length -gt 0) {
            Write-Host "--- $lf ---" -ForegroundColor DarkGray
            Get-Content $lf -Tail 20
        }
    }
}

# -- env vars so Claude Code / OpenCode talk to the local proxy -------------
# These are PROCESS-scoped (this PowerShell only). A Claude Code / Opus session
# in another terminal is completely unaffected - it never sees these.
$env:ANTHROPIC_BASE_URL                       = "http://127.0.0.1:$LLM_PROXY_PORT"
$env:ANTHROPIC_API_KEY                        = 'sk-ant-local-dummy-not-real'
$env:OPENAI_BASE_URL                          = "http://127.0.0.1:$LLM_PROXY_PORT/v1"
$env:OPENAI_API_BASE                          = "http://127.0.0.1:$LLM_PROXY_PORT/v1"
$env:OPENAI_API_KEY                           = 'dummy'
$env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = '1'
$env:CLAUDE_CODE_ENABLE_TELEMETRY             = '0'
$env:CLAUDE_CODE_ATTRIBUTION_HEADER           = '0'
# Truecolor + flicker-free animations, incl. when launched inside tmux/winmux
# (where Claude Code otherwise degrades to a choppy "reduced motion" render).
if (-not $env:COLORTERM) { $env:COLORTERM = 'truecolor' }
$env:CLAUDE_FORCE_SYNCHRONIZED_OUTPUT = '1'

# Isolate this instance's Claude config/state into a project-local dir so it
# shares NOTHING with the user's global ~/.claude (no shared credentials, history,
# todos or settings). This is what lets it run safely next to a logged-in Opus
# session in another terminal.
$env:CLAUDE_CONFIG_DIR = Join-Path $INSTALL_DIR '.claude-local'
New-Item -ItemType Directory -Force -Path $env:CLAUDE_CONFIG_DIR | Out-Null

$specLabel  = if ($SPEC_ENABLED) { 'ON (up to 2.5x faster)' } else { 'OFF' }
$mlockLabel = if ($extra -contains '--mlock') { 'ON' } else { 'OFF' }
Write-Host ""
Write-Host "========================================================" -ForegroundColor White
Write-Host "  Ready" -ForegroundColor Green
Write-Host "  Model   : $modelName  (running locally on your GPU)" -ForegroundColor Green
Write-Host "  Proxy   : http://127.0.0.1:$LLM_PROXY_PORT/v1"
Write-Host "  Server  : http://127.0.0.1:$LLAMA_PORT"
Write-Host "  Context : $ctx | Batch: $batch | GPU layers: $ngl"
Write-Host "  Threads : $threads/$totalCores | mlock: $mlockLabel | Spec: $specLabel"
Write-Host "  Metrics : http://127.0.0.1:$LLAMA_PORT/metrics"
Write-Host "  Config  : $env:CLAUDE_CONFIG_DIR (isolated from ~/.claude)"
Write-Host "========================================================" -ForegroundColor White
Write-Host "  This instance is sandboxed - your logged-in Claude Code" -ForegroundColor DarkGray
Write-Host "  (Opus) in other terminals is unaffected." -ForegroundColor DarkGray
Write-Host ""

try {
    switch ($mode) {
        '2' {
            Write-Host "[+] Launching Claude Code... (Ctrl+C or /exit to stop)" -ForegroundColor Green
            Write-Host "[i] Claude Code labels the model 'Sonnet 4.6' - that is the proxy alias" -ForegroundColor DarkGray
            Write-Host "    it requires. You are actually running $modelName locally." -ForegroundColor DarkGray
            Write-Host "    'API Usage Billing' just means a (dummy) API key is set; all traffic" -ForegroundColor DarkGray
            Write-Host "    stays on localhost - nothing is billed." -ForegroundColor DarkGray
            Write-Host ""
            & claude --model claude-sonnet-4-6
        }
        '3' {
            Write-Host "[+] Launching OpenCode..." -ForegroundColor Green
            & opencode
        }
        default { Write-Host "[x] Invalid mode" -ForegroundColor Red }
    }
} finally {
    Stop-Children
}
'@

    # Write WITH a UTF-8 BOM so Windows PowerShell 5.1 decodes it correctly.
    # (Without a BOM, 5.1 reads the file as Windows-1252 and any non-ASCII byte
    #  corrupts string parsing - the launcher would fail to even parse.)
    $utf8Bom = New-Object System.Text.UTF8Encoding($true)
    [System.IO.File]::WriteAllText($Launcher, ($header + $body), $utf8Bom)
    Log "Launcher written: $Launcher"
}

# -----------------------------------------------------------------------------
function Print-Summary {
    $sm = if ($SPEC_ENABLED) { 'ENABLED' } else { 'disabled' }
    Br
    Write-Host "+======================================================+" -ForegroundColor Green
    Write-Host "|  Installation complete!                              |" -ForegroundColor Green
    Write-Host "+======================================================+" -ForegroundColor Green
    Br
    Write-Host "  GPU:         $GPU_NAME (${TOTAL_RAM_GB} GB sys RAM)"
    Write-Host "  Context:     $CTX tokens"
    Write-Host "  Spec decode: $sm"
    Write-Host "  Launcher:    $Launcher"
    Br
    Write-Host "  To start:    powershell -ExecutionPolicy Bypass -File `"$Launcher`"" -ForegroundColor Cyan
    Br
    Write-Host "  OPTIMIZATIONS:" -ForegroundColor Yellow
    Write-Host "  - ATTRIBUTION_HEADER=0 - fixes 90% KV cache penalty"
    Write-Host "  - flash-attn + KV q8_0 - halves cache memory"
    Write-Host "  - batch 2048 - 2-3x faster prompt processing"
    if ($TOTAL_RAM_GB -ge 32) { Write-Host "  - mlock - prevents paging latency spikes" }
    else { Write-Host "  - mlock DISABLED (${TOTAL_RAM_GB} GB RAM, prevents freeze)" }
    if ($PRIO_SUPPORT) { Write-Host "  - prio 2 - reduces scheduling jitter" }
    if ($SPEC_ENABLED) { Write-Host "  - speculative decoding - up to 2.5x faster generation" }
    Br
    Write-Host "  Log: $Log"
    Br
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------
Step "Detecting hardware";          Detect-Hardware
Step "Selecting context size";      Pick-Context
Step "Checking dependencies";       Check-Deps
Step "Provisioning Python venv";    Setup-Venv
Step "Building llama.cpp";          Build-Llama
Step "Probing llama-server flags";  Detect-Flags
Step "Configuring Claude Code";     Configure-ClaudeSettings
Step "Hugging Face token";          Ask-HfToken
Step "Model selection";             Show-ModelMenu
Step "Downloading models";          Download-Models
Step "Speculative decoding";        Ask-Speculative
Step "Writing launcher";            Write-Launcher
Print-Summary
