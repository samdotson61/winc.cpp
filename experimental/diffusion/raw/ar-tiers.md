## AR 26B-A4B tier baselines 2026-06-12T12:57:27

### AR-T5 28GB full (-ngl 99 both)
args: -m C:\Claude\winc.cpp\models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf -fa on -ctk q8_0 -ctv q8_0 -p 2048 -n 64 -r 2 -d 0,16384 -ngl 99
ggml_cuda_init: found 2 CUDA devices (Total VRAM: 28590 MiB):
  Device 0: NVIDIA GeForce RTX 5070 Ti, compute capability 12.0, VMM: yes, VRAM: 16302 MiB
  Device 1: NVIDIA GeForce RTX 3060, compute capability 8.6, VMM: yes, VRAM: 12287 MiB
load_backend: loaded CUDA backend from C:\Claude\winc.cpp\bin\ggml-cuda.dll
load_backend: loaded RPC backend from C:\Claude\winc.cpp\bin\ggml-rpc.dll
load_backend: loaded CPU backend from C:\Claude\winc.cpp\bin\ggml-cpu-zen4.dll
| model                          |       size |     params | backend    | ngl | type_k | type_v |  fa |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | -----: | -----: | --: | --------------: | -------------------: |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 |          pp2048 |      3728.20 ± 18.80 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 |            tg64 |         78.81 ± 0.02 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | pp2048 @ d16384 |       2855.70 ± 0.51 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 |   tg64 @ d16384 |         70.45 ± 0.81 |

build: 4c6595503 (9601)
peakVRAM: GPU0=MB GPU1=MB  exit=0

### AR-T4 16GB (5070Ti solo, full offload)
args: -m C:\Claude\winc.cpp\models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf -fa on -ctk q8_0 -ctv q8_0 -p 2048 -n 64 -r 2 -d 0,16384 -ngl 99 -dev CUDA0
ggml_cuda_init: found 2 CUDA devices (Total VRAM: 28590 MiB):
  Device 0: NVIDIA GeForce RTX 5070 Ti, compute capability 12.0, VMM: yes, VRAM: 16302 MiB
  Device 1: NVIDIA GeForce RTX 3060, compute capability 8.6, VMM: yes, VRAM: 12287 MiB
load_backend: loaded CUDA backend from C:\Claude\winc.cpp\bin\ggml-cuda.dll
load_backend: loaded RPC backend from C:\Claude\winc.cpp\bin\ggml-rpc.dll
load_backend: loaded CPU backend from C:\Claude\winc.cpp\bin\ggml-cpu-zen4.dll
| model                          |       size |     params | backend    | ngl | type_k | type_v |  fa | dev          |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | -----: | -----: | --: | ------------ | --------------: | -------------------: |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA0        |          pp2048 |      5963.76 ± 17.49 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA0        |            tg64 |        152.54 ± 2.49 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA0        | pp2048 @ d16384 |      4580.36 ± 11.43 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA0        |   tg64 @ d16384 |        128.57 ± 0.25 |

build: 4c6595503 (9601)
peakVRAM: GPU0=MB GPU1=MB  exit=0

### AR-T3 12GB-class (3060 solo, half exps)
args: -m C:\Claude\winc.cpp\models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf -fa on -ctk q8_0 -ctv q8_0 -p 2048 -n 64 -r 2 -d 0,16384 -ngl 99 -dev CUDA1 -ot blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU
ggml_cuda_init: found 2 CUDA devices (Total VRAM: 28590 MiB):
  Device 0: NVIDIA GeForce RTX 5070 Ti, compute capability 12.0, VMM: yes, VRAM: 16302 MiB
  Device 1: NVIDIA GeForce RTX 3060, compute capability 8.6, VMM: yes, VRAM: 12287 MiB
load_backend: loaded CUDA backend from C:\Claude\winc.cpp\bin\ggml-cuda.dll
load_backend: loaded RPC backend from C:\Claude\winc.cpp\bin\ggml-rpc.dll
load_backend: loaded CPU backend from C:\Claude\winc.cpp\bin\ggml-cpu-zen4.dll
| model                          |       size |     params | backend    | ngl | type_k | type_v |  fa | dev          | ot                    |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | -----: | -----: | --: | ------------ | --------------------- | --------------: | -------------------: |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU |          pp2048 |        170.54 ± 0.18 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU |            tg64 |         34.83 ± 0.26 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU | pp2048 @ d16384 |        168.65 ± 0.42 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU |   tg64 @ d16384 |         31.99 ± 0.04 |

build: 4c6595503 (9601)
peakVRAM: GPU0=MB GPU1=MB  exit=0

### AR-T2 4GB-class (3060 solo, all exps CPU)
args: -m C:\Claude\winc.cpp\models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf -fa on -ctk q8_0 -ctv q8_0 -p 2048 -n 64 -r 2 -d 0,16384 -ngl 99 -dev CUDA1 -ot exps=CPU
ggml_cuda_init: found 2 CUDA devices (Total VRAM: 28590 MiB):
  Device 0: NVIDIA GeForce RTX 5070 Ti, compute capability 12.0, VMM: yes, VRAM: 16302 MiB
  Device 1: NVIDIA GeForce RTX 3060, compute capability 8.6, VMM: yes, VRAM: 12287 MiB
load_backend: loaded CUDA backend from C:\Claude\winc.cpp\bin\ggml-cuda.dll
load_backend: loaded RPC backend from C:\Claude\winc.cpp\bin\ggml-rpc.dll
load_backend: loaded CPU backend from C:\Claude\winc.cpp\bin\ggml-cpu-zen4.dll
| model                          |       size |     params | backend    | ngl | type_k | type_v |  fa | dev          | ot                    |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | -----: | -----: | --: | ------------ | --------------------- | --------------: | -------------------: |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              |          pp2048 |         91.21 ± 0.30 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              |            tg64 |         25.00 ± 0.05 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              | pp2048 @ d16384 |         97.13 ± 0.78 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  99 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              |   tg64 @ d16384 |         24.12 ± 0.12 |

build: 4c6595503 (9601)
peakVRAM: GPU0=MB GPU1=MB  exit=0

### AR-T1 2GB-class (half layers + exps CPU)
args: -m C:\Claude\winc.cpp\models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf -fa on -ctk q8_0 -ctv q8_0 -p 2048 -n 64 -r 2 -d 0,16384 -ngl 15 -dev CUDA1 -ot exps=CPU
ggml_cuda_init: found 2 CUDA devices (Total VRAM: 28590 MiB):
  Device 0: NVIDIA GeForce RTX 5070 Ti, compute capability 12.0, VMM: yes, VRAM: 16302 MiB
  Device 1: NVIDIA GeForce RTX 3060, compute capability 8.6, VMM: yes, VRAM: 12287 MiB
load_backend: loaded CUDA backend from C:\Claude\winc.cpp\bin\ggml-cuda.dll
load_backend: loaded RPC backend from C:\Claude\winc.cpp\bin\ggml-rpc.dll
load_backend: loaded CPU backend from C:\Claude\winc.cpp\bin\ggml-cpu-zen4.dll
| model                          |       size |     params | backend    | ngl | type_k | type_v |  fa | dev          | ot                    |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | -----: | -----: | --: | ------------ | --------------------- | --------------: | -------------------: |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  15 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              |          pp2048 |         80.54 ± 0.05 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  15 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              |            tg64 |         19.35 ± 1.09 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  15 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              | pp2048 @ d16384 |         84.37 ± 0.73 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |  15 |   q8_0 |   q8_0 |   1 | CUDA1        | exps=CPU              |   tg64 @ d16384 |         14.21 ± 0.84 |

build: 4c6595503 (9601)
peakVRAM: GPU0=MB GPU1=MB  exit=0

### AR-T0 CPU only
args: -m C:\Claude\winc.cpp\models\gemma-4-26B-A4B-it-UD-IQ4_NL.gguf -fa on -ctk q8_0 -ctv q8_0 -p 2048 -n 64 -r 2 -d 0,16384 -ngl 0
ggml_cuda_init: found 2 CUDA devices (Total VRAM: 28590 MiB):
  Device 0: NVIDIA GeForce RTX 5070 Ti, compute capability 12.0, VMM: yes, VRAM: 16302 MiB
  Device 1: NVIDIA GeForce RTX 3060, compute capability 8.6, VMM: yes, VRAM: 12287 MiB
load_backend: loaded CUDA backend from C:\Claude\winc.cpp\bin\ggml-cuda.dll
load_backend: loaded RPC backend from C:\Claude\winc.cpp\bin\ggml-rpc.dll
load_backend: loaded CPU backend from C:\Claude\winc.cpp\bin\ggml-cpu-zen4.dll
| model                          |       size |     params | backend    | ngl | type_k | type_v |  fa |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | -----: | -----: | --: | --------------: | -------------------: |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |   0 |   q8_0 |   q8_0 |   1 |          pp2048 |      684.59 ± 132.89 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |   0 |   q8_0 |   q8_0 |   1 |            tg64 |         15.50 ± 0.08 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |   0 |   q8_0 |   q8_0 |   1 | pp2048 @ d16384 |        812.60 ± 9.78 |
| gemma4 26B.A4B IQ4_NL - 4.5 bpw |  12.66 GiB |    25.23 B | CUDA       |   0 |   q8_0 |   q8_0 |   1 |   tg64 @ d16384 |         10.50 ± 0.05 |

build: 4c6595503 (9601)
peakVRAM: GPU0=MB GPU1=MB  exit=0

DONE 2026-06-12T13:13:36
