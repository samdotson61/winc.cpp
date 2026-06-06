# =============================================================================
# winc - tiny CLI for winc.cpp
#   winc ls                      list downloaded + available models
#   winc -d <alias|repo file>    download a model (HuggingFace)
#   winc -r <model>              delete a downloaded model
#   winc -s <app> <model>        start a sandboxed local instance with a model
#                                  app = claude | opencode | openclaw | cli
#   winc -c | winc check         check for updates (read-only)
#   winc -u | winc update        update llama.cpp + Python packages (+ pull source)
#   winc -n | winc uninstall     remove installed components (models, venv, engine)
#   winc help                    this help
# =============================================================================
$ErrorActionPreference = 'Stop'

$Root        = $PSScriptRoot
$ModelsDir   = Join-Path $Root 'models'
$VenvScripts = Join-Path $Root 'venv\Scripts'
$Launcher    = Join-Path $Root 'launcher.ps1'
. (Join-Path $Root 'catalog.ps1')   # -> $WINC_MODELS

function Say  { param($m) Write-Host $m }
function Good { param($m) Write-Host "[+] $m" -ForegroundColor Green }
function Warn { param($m) Write-Host "[!] $m" -ForegroundColor Yellow }
function Die  { param($m) Write-Host "[x] $m" -ForegroundColor Red; exit 1 }

function Resolve-Catalog {
    param($q)
    if (-not $q) { return $null }
    foreach ($m in $WINC_MODELS) {
        if ($m.Alias -ieq $q -or $m.File -ieq $q -or $m.Name -ieq $q) { return $m }
    }
    return $null
}

function Resolve-Downloaded {
    # Find a downloaded .gguf matching an alias/filename/substring. Returns FileInfo or $null.
    param($q)
    if (-not (Test-Path $ModelsDir)) { return $null }
    $entry = Resolve-Catalog $q
    $target = if ($entry) { $entry.File } else { $q }
    $files = Get-ChildItem -Path $ModelsDir -Filter '*.gguf' -File -ErrorAction SilentlyContinue
    foreach ($f in $files) { if ($f.Name -ieq $target -or $f.BaseName -ieq $target) { return $f } }
    foreach ($f in $files) { if ($f.Name -like "*$target*") { return $f } }
    return $null
}

function Find-VenvPython {
    $p = Join-Path $VenvScripts 'python.exe'
    if (Test-Path $p) { return $p }
    return $null
}

function Show-Usage {
    Say ""
    Say "winc - local Claude Code models (winc.cpp)"
    Say ""
    Say "  winc ls                       list downloaded + available models"
    Say "  winc -d <alias>               download a catalogue model"
    Say "  winc -d <repo> <file>         download any GGUF from HuggingFace"
    Say "  winc -r <model>               delete a downloaded model (-y to skip prompt)"
    Say "  winc -s claude <model>        start Claude Code on a local model (sandboxed)"
    Say "  winc -s opencode <model>      start OpenCode on a local model"
    Say "  winc -s openclaw <model>      start OpenClaw (terminal UI) on a local model"
    Say "  winc -s cli <model>           start the raw llama.cpp chat CLI"
    Say "  winc -c | winc check          check for updates (read-only, applies nothing)"
    Say "  winc -u | winc update         update llama.cpp + Python packages (and pull source)"
    Say "  winc -n | winc uninstall      remove installed components (-y to skip prompt)"
    Say "  winc help                     show this help"
    Say ""
    Say "  <model> is an alias (see 'winc ls') or part of a downloaded filename."
    Say ""
}

function Cmd-Ls {
    Say ""
    Say "Downloaded (in models\):"
    $files = @()
    if (Test-Path $ModelsDir) { $files = Get-ChildItem -Path $ModelsDir -Filter '*.gguf' -File | Sort-Object Name }
    if ($files.Count -eq 0) {
        Say "  (none yet - use 'winc -d <alias>')"
    } else {
        foreach ($f in $files) {
            $gb = [Math]::Round($f.Length / 1GB, 1)
            Say ("  {0,-46} {1,6} GB" -f $f.Name, $gb)
        }
    }
    Say ""
    Say "Available to download (alias  ~size  model):"
    $labels = @{ small = '6-8 GB GPUs'; mid = '16 GB GPUs'; large = '24 GB+ GPUs' }
    foreach ($tier in 'small','mid','large') {
        Say ""
        Say ("  -- $($labels[$tier]) --")
        foreach ($m in ($WINC_MODELS | Where-Object { $_.Tier -eq $tier })) {
            $have = Resolve-Downloaded $m.Alias
            $mark = if ($have) { '  [installed]' } else { '' }
            Say ("  {0,-18} {1,8}  {2}{3}" -f $m.Alias, $m.Size, $m.Name, $mark)
        }
    }
    Say ""
    Say "Download:  winc -d <alias>      Start:  winc -s claude <alias>"
    Say ""
}

function Cmd-Download {
    param($rest)
    $rest = @($rest)   # never let a single arg arrive as a scalar string
    if (-not $rest -or $rest.Count -eq 0) { Die "Usage: winc -d <alias>   or   winc -d <repo> <file>" }
    $py = Find-VenvPython
    if (-not $py) { Die "venv not found. Run install.cmd first." }

    if ($rest.Count -ge 2) {
        $repo = $rest[0]; $file = $rest[1]
    } else {
        $entry = Resolve-Catalog $rest[0]
        if (-not $entry) { Die "Unknown model '$($rest[0])'. Run 'winc ls' for aliases, or pass '<repo> <file>'." }
        $repo = $entry.Repo; $file = $entry.File
    }

    $target = Join-Path $ModelsDir $file
    if (Test-Path $target) { Good "Already downloaded: $file"; return }
    New-Item -ItemType Directory -Force -Path $ModelsDir | Out-Null
    Good "Downloading $file"
    Say  "  from $repo"
    # Use the venv python + hf_get.py (clean 1s progress bar + ETA), not the
    # hf.exe shim (which breaks if the install folder is renamed).
    & $py (Join-Path $Root 'hf_get.py') $repo $file $ModelsDir
    if ($LASTEXITCODE -ne 0) { Die "Download failed. For gated models set HF_TOKEN (`$env:HF_TOKEN='hf_...') and retry." }
    if (Test-Path $target) { Good "Done: $file" } else { Warn "Download reported success but $file is not in models\ - check the filename." }
}

function Cmd-Start {
    param($rest)
    $rest = @($rest)   # never let args arrive as a scalar string
    if (-not $rest -or $rest.Count -lt 2) { Die "Usage: winc -s <claude|opencode|openclaw|cli> <model>" }
    $app = "$($rest[0])".ToLower()
    $modelQ = $rest[1]
    $mode = switch ($app) {
        'claude'   { '2' }
        'opencode' { '3' }
        'openclaw' { '4' }
        'cli'      { '1' }
        default    { Die "Unknown app '$app'. Use claude, opencode, openclaw, or cli." }
    }
    if (-not (Test-Path $Launcher)) { Die "launcher.ps1 not found. Run install.cmd first." }

    $file = Resolve-Downloaded $modelQ
    if (-not $file) {
        $entry = Resolve-Catalog $modelQ
        if ($entry) { Die "'$($entry.Alias)' is not downloaded yet. Run:  winc -d $($entry.Alias)" }
        Die "No downloaded model matches '$modelQ'. See 'winc ls'."
    }

    Good "Starting $app on $($file.Name) (sandboxed local instance)"
    $env:WINC_MODEL = $file.Name
    $env:WINC_MODE  = $mode
    try { & $Launcher } finally { Remove-Item Env:\WINC_MODEL, Env:\WINC_MODE -ErrorAction SilentlyContinue }
}

function Cmd-Remove {
    param($rest)
    $rest = @($rest)   # never let a single arg arrive as a scalar string
    if (-not $rest -or $rest.Count -eq 0) { Die "Usage: winc -r <alias|filename>   (add -y to skip the prompt)" }
    $yes = $rest -contains '-y' -or $rest -contains '--yes'
    $q   = @($rest | Where-Object { $_ -ne '-y' -and $_ -ne '--yes' })[0]
    if (-not $q) { Die "Usage: winc -r <alias|filename>" }

    $file = Resolve-Downloaded $q
    if (-not $file) {
        $entry = Resolve-Catalog $q
        if ($entry) { Die "'$($entry.Alias)' is not downloaded - nothing to remove." }
        Die "No downloaded model matches '$q'. See 'winc ls'."
    }
    $gb = [Math]::Round($file.Length / 1GB, 1)
    if (-not $yes) {
        $ans = Read-Host "Delete $($file.Name) ($gb GB)? [y/N]"
        if ($ans -notmatch '^[yY]') { Say "Cancelled - nothing removed."; return }
    }
    Remove-Item $file.FullName -Force
    Good "Removed: $($file.Name) ($gb GB freed)"
}

function Cmd-Update {
    # 1) Update winc.cpp's own scripts if this is a git clone with a remote.
    if (Test-Path (Join-Path $Root '.git')) {
        $remote = $null
        try { $remote = (& git -C $Root remote 2>$null | Select-Object -First 1) } catch {}
        if ($remote) {
            Good "Updating winc.cpp source (git pull)..."
            & git -C $Root pull --ff-only
            if ($LASTEXITCODE -ne 0) { Warn "git pull failed (local changes or no upstream) - continuing with component update." }
        } else {
            Say "No git remote set - skipping source pull, updating components only."
        }
    }
    # 2) Update components (llama.cpp rebuild + Python packages + launcher) via
    #    the installer's -Update path. Run in a fresh PowerShell, like install.cmd.
    $install = Join-Path $Root 'install.ps1'
    if (-not (Test-Path $install)) { Die "install.ps1 not found - is this a complete winc.cpp folder?" }
    Good "Updating components (llama.cpp rebuild + Python packages)..."
    & powershell -NoProfile -ExecutionPolicy Bypass -File $install -Update
    if ($LASTEXITCODE -ne 0) { Die "Update failed (exit $LASTEXITCODE) - see $Root\install.log" }
    Good "Update finished."
}

function Cmd-Check {
    # Read-only: fetch upstream and report what 'winc -u' would change. Relax the
    # error preference so native git/pip stderr (progress) can't abort the check.
    $prevEAP = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try {
        Say ""
        Say "Checking for updates (read-only)..."
        Say ""
        $any = $false
        $haveGit = [bool](Get-Command git -ErrorAction SilentlyContinue)

        # 1) winc.cpp source
        if ($haveGit -and (Test-Path (Join-Path $Root '.git'))) {
            $remote = (& git -C $Root remote 2>$null | Select-Object -First 1)
            if ($remote) {
                & git -C $Root fetch --quiet 2>$null | Out-Null
                $u = (& git -C $Root rev-parse --abbrev-ref '@{u}' 2>$null)
                if ($LASTEXITCODE -eq 0 -and $u) {
                    $behind = (& git -C $Root rev-list --count "HEAD..$u" 2>$null)
                    if ($behind -match '^\d+$' -and [int]$behind -gt 0) { Warn "winc.cpp source : $behind update(s) available"; $any = $true }
                    else { Good "winc.cpp source : up to date" }
                } else { Say "  winc.cpp source : no upstream branch set (can't compare)" }
            } else { Say "  winc.cpp source : no git remote configured (skipped)" }
        } else { Say "  winc.cpp source : not a git clone (skipped)" }

        # 2) llama.cpp engine (shallow clone -> compare HEAD vs fetched master tip)
        $llama = Join-Path $Root 'llama.cpp'
        if ($haveGit -and (Test-Path (Join-Path $llama '.git'))) {
            & git -C $llama fetch --quiet origin master 2>$null | Out-Null
            if ($LASTEXITCODE -eq 0) {
                $localRev  = (& git -C $llama rev-parse HEAD 2>$null)
                $remoteRev = (& git -C $llama rev-parse FETCH_HEAD 2>$null)
                if ($localRev -and $remoteRev) {
                    if ($localRev -ne $remoteRev) { Warn "llama.cpp engine: new commits upstream (rebuild on update)"; $any = $true }
                    else { Good "llama.cpp engine: up to date" }
                } else { Say "  llama.cpp engine: couldn't compare revisions" }
            } else { Say "  llama.cpp engine: fetch failed (offline?)" }
        } else { Say "  llama.cpp engine: not built yet (skipped)" }

        # 3) Python packages (litellm + huggingface_hub) vs PyPI
        $py = Find-VenvPython
        if ($py) {
            Say "  Python packages: querying PyPI..."
            $out = & $py -m pip list --outdated --disable-pip-version-check 2>$null
            $watch = @('litellm', 'huggingface-hub')
            $hits = @()
            foreach ($line in $out) {
                foreach ($w in $watch) { if ($line -match ("^{0}\s" -f [regex]::Escape($w))) { $hits += (($line -replace '\s{2,}', ' ').Trim()) } }
            }
            if ($hits.Count -gt 0) { Warn "Python packages: updates available:"; $hits | ForEach-Object { Say "    $_" }; $any = $true }
            else { Good "Python packages: litellm + huggingface_hub up to date" }
        } else { Say "  Python packages: venv not present (skipped)" }

        Say ""
        if ($any) { Say "Updates available - run 'winc -u' (or 'winc update') to apply." }
        else      { Say "Everything is up to date." }
        Say ""
    } finally { $ErrorActionPreference = $prevEAP }
}

function Cmd-Uninstall {
    param($rest)
    $rest = @($rest)
    $yes = ($rest -contains '-y') -or ($rest -contains '--yes')

    # Everything the installer creates INSIDE the winc.cpp folder. The source
    # scripts (winc.ps1, install.ps1, catalog.ps1, *.py, *.cmd, README) are left
    # in place so you can re-run install.cmd - or delete the folder by hand to
    # remove those too. We can't delete the folder itself here: this script is
    # running from it.
    $targets = @(
        @{ Path = $ModelsDir;                             Label = 'models\ (downloaded GGUFs)' },
        @{ Path = (Join-Path $Root 'llama.cpp');          Label = 'llama.cpp\ (engine + build)' },
        @{ Path = (Join-Path $Root 'venv');               Label = 'venv\ (Python environment)' },
        @{ Path = (Join-Path $Root '.claude-local');      Label = '.claude-local\ (sandboxed config)' },
        @{ Path = $Launcher;                              Label = 'launcher.ps1' },
        @{ Path = (Join-Path $Root '.winc-timings.json'); Label = '.winc-timings.json' },
        @{ Path = (Join-Path $Root 'install.log');        Label = 'install.log' },
        @{ Path = (Join-Path $Root 'pip.log');            Label = 'pip.log' },
        @{ Path = (Join-Path $Root '__pycache__');        Label = '__pycache__\' }
    )

    $present = @()
    $totalBytes = 0.0
    foreach ($t in $targets) {
        if (-not (Test-Path $t.Path)) { continue }
        $bytes = 0.0
        try {
            if (Test-Path $t.Path -PathType Container) {
                $sum = (Get-ChildItem $t.Path -Recurse -File -Force -ErrorAction SilentlyContinue | Measure-Object -Property Length -Sum).Sum
                if ($sum) { $bytes = [double]$sum }
            } else {
                $bytes = [double]((Get-Item $t.Path -Force -ErrorAction SilentlyContinue).Length)
            }
        } catch {}
        $totalBytes += $bytes
        $present += @{ Path = $t.Path; Label = $t.Label; Bytes = $bytes }
    }

    $userPath = [Environment]::GetEnvironmentVariable('PATH', 'User')
    $onPath = [bool]($userPath -and (($userPath -split ';') -contains $Root))

    if ($present.Count -eq 0 -and -not $onPath) {
        Good "Nothing to uninstall - no installed components found in $Root."
        return
    }

    Say ""
    Say "This will remove the following from:  $Root"
    Say ""
    foreach ($p in $present) {
        $gb = [Math]::Round($p.Bytes / 1GB, 2)
        Say ("  {0,-42} {1,8} GB" -f $p.Label, $gb)
    }
    if ($onPath) { Say "  (also removes winc.cpp from your user PATH)" }
    Say ""
    Say ("  Total to free: {0} GB" -f [Math]::Round($totalBytes / 1GB, 2))
    Say ""
    Say "  Source scripts (winc.ps1, install.ps1, catalog.ps1, *.py, *.cmd, README)"
    Say "  are kept so you can re-run install.cmd. Delete the folder by hand to"
    Say "  remove those too."
    Say ""

    if (-not $yes) {
        $ans = Read-Host "Uninstall winc.cpp? [y/N]"
        if ($ans -notmatch '^[yY]') { Say "Cancelled - nothing removed."; return }
    }

    foreach ($p in $present) {
        try {
            Remove-Item $p.Path -Recurse -Force -ErrorAction Stop
            Good "Removed: $($p.Label)"
        } catch {
            Warn "Could not remove $($p.Label): $($_.Exception.Message)"
        }
    }

    if ($onPath) {
        try {
            $new = (($userPath -split ';') | Where-Object { $_ -and $_ -ne $Root }) -join ';'
            [Environment]::SetEnvironmentVariable('PATH', $new, 'User')
            Good "Removed winc.cpp from user PATH (open a new terminal to refresh)."
        } catch {
            Warn "Could not update user PATH: $($_.Exception.Message)"
        }
    }

    Say ""
    Good "Uninstall complete."
    Say "  To remove the source too, delete:  $Root"
    Say ""
}

# -- dispatch ----------------------------------------------------------------
# NOTE: build $rest with an explicit @() wrapper. A one-element slice returned
# from an `if {}` block gets unwrapped to a scalar string, and then $rest[0]
# would index the STRING (e.g. 'qwen3.6-35b'[0] -> 'q'). @() keeps it an array.
$cmd  = ''
$rest = @()
if ($args.Count -ge 1) { $cmd = "$($args[0])".ToLower() }
if ($args.Count -ge 2) { $rest = @($args[1..($args.Count - 1)]) }

switch ($cmd) {
    'ls'        { Cmd-Ls }
    'list'      { Cmd-Ls }
    '-d'        { Cmd-Download $rest }
    'download'  { Cmd-Download $rest }
    '-r'        { Cmd-Remove $rest }
    'rm'        { Cmd-Remove $rest }
    'remove'    { Cmd-Remove $rest }
    '-s'        { Cmd-Start $rest }
    'start'     { Cmd-Start $rest }
    '-u'        { Cmd-Update }
    'update'    { Cmd-Update }
    '-c'        { Cmd-Check }
    'check'     { Cmd-Check }
    '-n'        { Cmd-Uninstall $rest }
    'uninstall' { Cmd-Uninstall $rest }
    'help'      { Show-Usage }
    '-h'        { Show-Usage }
    '--help'    { Show-Usage }
    ''          { Show-Usage }
    default     { Warn "Unknown command '$cmd'"; Show-Usage; exit 1 }
}
