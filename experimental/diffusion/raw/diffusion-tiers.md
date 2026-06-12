## DiffusionGemma tier matrix 2026-06-12T13:19:47

### DG-T5 28GB full (-ngl 99 both)
extra: -ngl 99
diffusion step: 18/48 [==================                                ] 37%0.40.799.480 W ~llama_context:      CUDA0 compute buffer size of 1586.9639 MiB, does not match expectation of 1406.0156 MiB
0.40.799.483 W ~llama_context:  CUDA_Host compute buffer size of 1499.5092 MiB, does not match expectation of 1062.0469 MiB
total time: 26801.78ms, time per step: 788.29ms (34 steps over 2 blocks, entropy-bound)
throughput: 19.1 tok/s (512 tok in 26801.78ms), in-step parallel 325 tok/s (256-tok canvas x 17.0 steps/block)
replyTail:     ```      *   Respond ONLY with JSON? Yes.<channel|>{   "score": total time: 26801.78ms, time per step: 788.29ms (34 steps over 2 blocks, entropy-bound) throughput: 19.1 tok/s (512 tok in 26801.78ms), in-step parallel 325 tok/s (256-tok canvas x 17.0 steps/block)
peakVRAM: GPU0=13074MB GPU1=8083MB wall=42.9s exit=0

### DG-T4 16GB (5070Ti solo, full offload)
extra: -ngl 99 -dev CUDA0
0.01.825.627 E ggml_backend_cuda_buffer_type_alloc_buffer: allocating 16013.20 MiB on device 0: cudaMalloc failed: out of memory
0.01.825.632 E alloc_tensor_range: failed to allocate CUDA0 buffer of size 16791060096
0.01.920.581 E llama_model_load: error loading model: unable to allocate CUDA0 buffer
0.01.920.587 E llama_model_load_from_file_impl: failed to load model
0.01.920.588 E error: failed to load model 'C:\Claude\winc.cpp\models\diffusiongemma-26B-A4B-it-Q4_K_M.gguf'
replyTail: 0.00.505.330 W load: special_eog_ids contains '<|tool_response>', removing '</s>' token from EOG list 0.01.825.627 E ggml_backend_cuda_buffer_type_alloc_buffer: allocating 16013.20 MiB on device 0: cudaMalloc failed: out of memory 0.01.825.632 E alloc_tensor_range: failed to allocate CUDA0 buffer of
peakVRAM: GPU0=7484MB GPU1=0MB wall=2s exit=1

### DG-T4b 16GB (5070Ti solo, all exps CPU)
extra: -ngl 99 -dev CUDA0 -ot exps=CPU
total time: 39762.53ms, time per step: 1019.55ms (39 steps over 2 blocks, entropy-bound)
throughput: 12.9 tok/s (512 tok in 39762.53ms), in-step parallel 251 tok/s (256-tok canvas x 19.5 steps/block)
replyTail:     ```      *   Respond ONLY with JSON.<channel|>{"score": 75, "reasons": ["Strong Figma skills match technical needs", "Bilingual status adds competitive value in Austin", "Recent graduate total time: 39762.53ms, time per step: 1019.55ms (39 steps over 2 blocks, entropy-bound) throughput: 12.9 tok
peakVRAM: GPU0=4920MB GPU1=0MB wall=43.9s exit=0

### DG-T3 12GB-class (3060 solo, half exps)
extra: -ngl 99 -dev CUDA1 -ot blk\.(0|1|2|3|4|5|6|7|8|9|1[0-4])\.ffn_.*_exps.*=CPU
total time: 161071.66ms, time per step: 5033.49ms (32 steps over 2 blocks, entropy-bound)
throughput: 3.2 tok/s (512 tok in 161071.66ms), in-step parallel 51 tok/s (256-tok canvas x 16.0 steps/block)
replyTail:   "score": 80,   "reasons": [     "Strong total time: 161071.66ms, time per step: 5033.49ms (32 steps over 2 blocks, entropy-bound) throughput: 3.2 tok/s (512 tok in 161071.66ms), in-step parallel 51 tok/s (256-tok canvas x 16.0 steps/block) diffusion step: 14/48 [==============                     
peakVRAM: GPU0=636MB GPU1=11523MB wall=171.8s exit=0

### DG-T2 4GB-class (3060 solo, all exps CPU)
extra: -ngl 99 -dev CUDA1 -ot exps=CPU
total time: 286812.30ms, time per step: 8962.88ms (32 steps over 2 blocks, entropy-bound)
throughput: 1.8 tok/s (512 tok in 286812.30ms), in-step parallel 29 tok/s (256-tok canvas x 16.0 steps/block)
replyTail:   "score": 80,   "reasons": [     "Strong total time: 286812.30ms, time per step: 8962.88ms (32 steps over 2 blocks, entropy-bound) throughput: 1.8 tok/s (512 tok in 286812.30ms), in-step parallel 29 tok/s (256-tok canvas x 16.0 steps/block) diffusion step: 14/48 [==============                     
peakVRAM: GPU0=636MB GPU1=4307MB wall=292.5s exit=0

### DG-T1 2GB-class (half layers + exps CPU)
extra: -ngl 15 -dev CUDA1 -ot exps=CPU
total time: 316942.58ms, time per step: 10223.95ms (31 steps over 2 blocks, entropy-bound)
throughput: 1.6 tok/s (512 tok in 316942.58ms), in-step parallel 25 tok/s (256-tok canvas x 15.5 steps/block)
replyTail:     "Bilingual skills provide added value",     "Educational background aligns with junior-level requirements"   ], total time: 316942.58ms, time per step: 10223.95ms (31 steps over 2 blocks, entropy-bound) throughput: 1.6 tok/s (512 tok in 316942.58ms), in-step parallel 25 tok/s (256-tok canvas x 1
peakVRAM: GPU0=636MB GPU1=3063MB wall=321.6s exit=0

### DG-T0 CPU only
extra: -ngl 0
total time: 40304.86ms, time per step: 1221.36ms (33 steps over 2 blocks, entropy-bound)
throughput: 12.7 tok/s (512 tok in 40304.86ms), in-step parallel 210 tok/s (256-tok canvas x 16.5 steps/block)
replyTail:     ```      *   Respond ONLY with JSON? Yes.<channel|> total time: 40304.86ms, time per step: 1221.36ms (33 steps over 2 blocks, entropy-bound) throughput: 12.7 tok/s (512 tok in 40304.86ms), in-step parallel 210 tok/s (256-tok canvas x 16.5 steps/block) diffusion step: 16/48 [================     
peakVRAM: GPU0=2636MB GPU1=93MB wall=44.5s exit=0

DONE 2026-06-12T13:35:07
