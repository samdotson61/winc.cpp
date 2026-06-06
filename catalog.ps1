# =============================================================================
# Shared model catalog for winc.cpp
# Sourced by both install.ps1 (the menu) and winc.ps1 (the CLI).
# Each entry: N (menu index), Alias (for `winc -d` / `winc -s`), Name, Size,
#             Repo + File (HuggingFace), Note.
# Curated to fit a 16 GB NVIDIA GPU with 65K context (flash-attn + q8_0 KV).
# =============================================================================
$WINC_MODELS = @(
    @{ N=1; Alias='qwen3.6-27b';  Name='Qwen3.6-27B Q3_K_M';                    Size='13.6 GB'; Note='Newest top dense coder (2026). SWE-bench ~77%. Clean 16 GB fit';
       Repo='unsloth/Qwen3.6-27B-GGUF';                       File='Qwen3.6-27B-Q3_K_M.gguf' },
    @{ N=2; Alias='qwen3.6-35b';  Name='Qwen3.6-35B-A3B UD-IQ3_S';              Size='13.7 GB'; Note='Newest MoE coder (3B active = fast). Agentic/multi-file work';
       Repo='unsloth/Qwen3.6-35B-A3B-GGUF';                   File='Qwen3.6-35B-A3B-UD-IQ3_S.gguf' },
    @{ N=3; Alias='gpt-oss-20b';  Name='GPT-OSS-20B (MXFP4)';                   Size='12.1 GB'; Note='OpenAI open-weights. Fast all-round daily driver';
       Repo='ggml-org/gpt-oss-20b-GGUF';                      File='gpt-oss-20b-mxfp4.gguf' },
    @{ N=4; Alias='devstral';     Name='Devstral-Small-2507 Q4_K_M';           Size='14.3 GB'; Note='Mistral 24B agent/coding model. Strong tool-calling';
       Repo='bartowski/mistralai_Devstral-Small-2507-GGUF';   File='mistralai_Devstral-Small-2507-Q4_K_M.gguf' },
    @{ N=5; Alias='qwen3-14b';    Name='Qwen3-14B Q5_K_M';                     Size='10.5 GB'; Note='Smaller dense Qwen3. Snappy general/reasoning + tools';
       Repo='bartowski/Qwen_Qwen3-14B-GGUF';                  File='Qwen_Qwen3-14B-Q5_K_M.gguf' },
    @{ N=6; Alias='mistral-small';Name='Mistral-Small-3.2-24B-Instruct Q4_K_M';Size='14.3 GB'; Note='Strong general-purpose + tool use';
       Repo='bartowski/mistralai_Mistral-Small-3.2-24B-Instruct-2506-GGUF';
       File='mistralai_Mistral-Small-3.2-24B-Instruct-2506-Q4_K_M.gguf' },
    @{ N=7; Alias='gemma3-27b';   Name='Gemma-3-27B-IT Q3_K_M';                Size='13.5 GB'; Note='Google Gemma 3 27B. Multilingual, 128K ctx';
       Repo='bartowski/google_gemma-3-27b-it-GGUF';           File='google_gemma-3-27b-it-Q3_K_M.gguf' },
    @{ N=8; Alias='phi4';         Name='Phi-4-reasoning-plus (14B) Q5_K_M';    Size='10.5 GB'; Note='Microsoft Phi-4 RL-tuned. Excellent math/logic';
       Repo='bartowski/microsoft_Phi-4-reasoning-plus-GGUF';  File='microsoft_Phi-4-reasoning-plus-Q5_K_M.gguf' },
    @{ N=9; Alias='deepseek-r1';  Name='DeepSeek-R1-0528-Qwen3-8B Q6_K';       Size='6.5 GB';  Note='R1 reasoning distill. Best for math-heavy / algorithmic';
       Repo='bartowski/deepseek-ai_DeepSeek-R1-0528-Qwen3-8B-GGUF';
       File='deepseek-ai_DeepSeek-R1-0528-Qwen3-8B-Q6_K.gguf' }
)
