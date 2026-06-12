# DiffusionGemma 26B-A4B tier matrix via llama-diffusion-cli (PR 24423 build).
# Mirrors bench-ar.ps1 tier shapes; per-run VRAM peaks sampled to a file.
$ErrorActionPreference = "Continue"
$cli = "C:\Claude\llamacpp-diffusion\build\bin\llama-diffusion-cli.exe"
$m   = "C:\Claude\winc.cpp\models\diffusiongemma-26B-A4B-it-Q4_K_M.gguf"
$out = "$env:TEMP\ffn-bench\diffusion-tiers.md"
"## DiffusionGemma tier matrix $(Get-Date -Format s)" | Set-Content $out

$prompt = "Evaluate this job posting for a junior graphic designer in Austin TX. Salary 52000 USD, hybrid 3 days onsite, requires 2 years experience, Adobe suite, some Figma, portfolio required, benefits include health and dental. The candidate is a recent design graduate with strong Figma skills, one internship, and a bilingual English Spanish background. Respond ONLY with JSON: {""score"": 0-100, ""reasons"": [3 short strings], ""missing"": [skills gaps]}."

function Run-Tier([string]$label, [string[]]$extra) {
    Add-Content $out "`n### $label"
    Add-Content $out "extra: $($extra -join ' ')"
    $peakFile = "$env:TEMP\ffn-bench\vram-peak.txt"
    Remove-Item $peakFile -ErrorAction SilentlyContinue
    $sampler = Start-Job -ArgumentList $peakFile -ScriptBlock {
        param($pf)
        $max = @(0, 0)
        while ($true) {
            $u = & nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits 2>$null
            if ($u) { $i = 0; foreach ($line in @($u)) { $v = [int]$line; if ($v -gt $max[$i]) { $max[$i] = $v }; $i++ } }
            Set-Content $pf ($max -join ",")
            Start-Sleep -Milliseconds 1500
        }
    }
    $base = @("-m",$m,"--temp","0","--seed","31337","-fa","on","-c","4096","-ub","1024","-b","1024",
              "--diffusion-blocks","2","-p",$prompt)
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $r = & $cli @base @extra 2>&1 | ForEach-Object { "$_" }
    $sw.Stop()
    Stop-Job $sampler -ErrorAction SilentlyContinue; Remove-Job $sampler -Force -ErrorAction SilentlyContinue
    $peak = (Get-Content $peakFile -ErrorAction SilentlyContinue) -split ","
    # keep the stats + a sample of the reply, not the whole denoise spam
    $stats = $r | Select-String -Pattern "throughput|time per step|total time|error|failed|unable|CUDA|loaded|offload" | ForEach-Object { "$_" } | Select-Object -First 12
    Add-Content $out ($stats -join "`n")
    $reply = ($r | Select-Object -Last 6) -join " "
    Add-Content $out "replyTail: $($reply.Substring(0, [Math]::Min(300, $reply.Length)))"
    Add-Content $out "peakVRAM: GPU0=$($peak[0])MB GPU1=$($peak[1])MB wall=$([math]::Round($sw.Elapsed.TotalSeconds,1))s exit=$LASTEXITCODE"
}

$expsAll  = "exps=CPU"
$expsHalf = "blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU"

Run-Tier "DG-T5 28GB full (-ngl 99 both)"             @("-ngl","99")
Run-Tier "DG-T4 16GB (5070Ti solo, full offload)"     @("-ngl","99","-dev","CUDA0")
Run-Tier "DG-T4b 16GB (5070Ti solo, all exps CPU)"    @("-ngl","99","-dev","CUDA0","-ot",$expsAll)
Run-Tier "DG-T3 12GB-class (3060 solo, half exps)"    @("-ngl","99","-dev","CUDA1","-ot",$expsHalf)
Run-Tier "DG-T2 4GB-class (3060 solo, all exps CPU)"  @("-ngl","99","-dev","CUDA1","-ot",$expsAll)
Run-Tier "DG-T1 2GB-class (half layers + exps CPU)"   @("-ngl","15","-dev","CUDA1","-ot",$expsAll)
Run-Tier "DG-T0 CPU only"                             @("-ngl","0")

Add-Content $out "`nDONE $(Get-Date -Format s)"
