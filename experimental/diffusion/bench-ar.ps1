# AR baseline: gemma-4-26B-A4B-it (autoregressive) across VRAM-tier shapes.
# Each run samples nvidia-smi for peak VRAM so configs map to honest tiers.
$ErrorActionPreference = "Continue"
$bench = "C:\Claude\winc.cpp\bin\llama-bench.exe"
$m     = "C:\Claude\winc.cpp\models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf"
$out   = "$env:TEMP\ffn-bench\ar-tiers.md"
"## AR 26B-A4B tier baselines $(Get-Date -Format s)" | Set-Content $out

function Run-Tier([string]$label, [string[]]$benchArgs) {
    Add-Content $out "`n### $label"
    Add-Content $out "args: $($benchArgs -join ' ')"
    $sampler = Start-Job -ScriptBlock {
        $max = @(0, 0)
        while ($true) {
            $u = nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits 2>$null
            if ($u) { $i = 0; foreach ($line in $u) { $v = [int]$line; if ($v -gt $max[$i]) { $max[$i] = $v }; $i++ } }
            Start-Sleep -Milliseconds 1500
            if ((Get-Date) -gt $using:deadline) { break }
        }
        $max
    }
    $r = & $bench @benchArgs 2>&1
    Stop-Job $sampler -ErrorAction SilentlyContinue
    $peak = Receive-Job $sampler -ErrorAction SilentlyContinue
    Remove-Job $sampler -Force -ErrorAction SilentlyContinue
    Add-Content $out (($r | ForEach-Object { "$_" }) -join "`n")
    Add-Content $out "peakVRAM: GPU0=$($peak[0])MB GPU1=$($peak[1])MB  exit=$LASTEXITCODE"
}
$deadline = (Get-Date).AddHours(2)

$c = @("-m",$m,"-fa","on","-ctk","q8_0","-ctv","q8_0","-p","2048","-n","64","-r","2","-d","0,16384")
$expsAll  = "exps=CPU"
$expsHalf = "blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU"

Run-Tier "AR-T5 28GB full (-ngl 99 both)"            ($c + @("-ngl","99"))
Run-Tier "AR-T4 16GB (5070Ti solo, full offload)"    ($c + @("-ngl","99","-dev","CUDA0"))
Run-Tier "AR-T3 12GB-class (3060 solo, half exps)"   ($c + @("-ngl","99","-dev","CUDA1","-ot",$expsHalf))
Run-Tier "AR-T2 4GB-class (3060 solo, all exps CPU)" ($c + @("-ngl","99","-dev","CUDA1","-ot",$expsAll))
Run-Tier "AR-T1 2GB-class (half layers + exps CPU)"  ($c + @("-ngl","15","-dev","CUDA1","-ot",$expsAll))
Run-Tier "AR-T0 CPU only"                            ($c + @("-ngl","0"))

Add-Content $out "`nDONE $(Get-Date -Format s)"
