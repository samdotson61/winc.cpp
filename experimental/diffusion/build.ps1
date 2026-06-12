# Build llama-diffusion-cli from draft PR #24423 with CUDA.
# Engine clone lives outside the winc repo. Requires: git, cmake, ninja,
# MSVC Build Tools (vcvars64), CUDA toolkit.
$ErrorActionPreference = "Stop"
$src   = "C:\Claude\llamacpp-diffusion"
$ninja = "C:\Claude\diffusion-exp"

if (-not (Test-Path $src)) {
    git clone --filter=blob:none https://github.com/ggml-org/llama.cpp $src
    git -C $src fetch origin pull/24423/head:diffusion-pr
    git -C $src checkout diffusion-pr
}

# Ninja generator avoids needing CUDA's MSBuild integration; vcvars64 supplies cl.
$vcvars = "C:\Program Files (x86)\Microsoft Visual Studio\18\BuildTools\VC\Auxiliary\Build\vcvars64.bat"
$build = @"
call "$vcvars" >nul 2>&1
set PATH=$ninja;%PATH%
cmake -S $src -B $src\build -G Ninja -DCMAKE_BUILD_TYPE=Release -DGGML_CUDA=ON -DLLAMA_CURL=OFF
cmake --build $src\build --target llama-diffusion-cli
"@
$tmp = "$env:TEMP\diffusion-build.cmd"
Set-Content $tmp $build -Encoding ascii
cmd /c $tmp
