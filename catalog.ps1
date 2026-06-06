# =============================================================================
# Shared model catalog for winc.cpp
# Sourced by both install.ps1 (the menu) and winc.ps1 (the CLI).
# Each entry:
#   Tier  = vram bucket: 'small' (6-8 GB), 'mid' (16 GB), 'large' (24 GB+)
#   Alias = short name for `winc -d` / `winc -s` / `winc -r`
#   Name, Size, Repo + File (HuggingFace), Note
# Quants are chosen to fit each tier's VRAM with a useful context window.
# Order within a tier is best-first (the first entry is the recommended default).
# =============================================================================
$WINC_MODELS = @(
    # --- small: 6-8 GB GPUs -------------------------------------------------
    @{ Tier='small'; Alias='qwen2.5-coder-7b'; Name='Qwen2.5-Coder-7B-Instruct Q4_K_M'; Size='4.7 GB'; Note='Best small coder. Fast, fits 6-8 GB';
       Repo='bartowski/Qwen2.5-Coder-7B-Instruct-GGUF';        File='Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf' },
    @{ Tier='small'; Alias='deepseek-r1-8b';   Name='DeepSeek-R1-0528-Qwen3-8B Q4_K_M'; Size='5.0 GB'; Note='Fast reasoning distill. Math/algorithmic';
       Repo='bartowski/deepseek-ai_DeepSeek-R1-0528-Qwen3-8B-GGUF'; File='deepseek-ai_DeepSeek-R1-0528-Qwen3-8B-Q4_K_M.gguf' },
    @{ Tier='small'; Alias='llama3.1-8b';      Name='Llama-3.1-8B-Instruct Q4_K_M';     Size='4.9 GB'; Note='Meta 8B. Well-rounded, 128K ctx';
       Repo='bartowski/Meta-Llama-3.1-8B-Instruct-GGUF';       File='Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf' },
    @{ Tier='small'; Alias='gemma4-e4b';       Name='Gemma-4-E4B-IT Q4_K_M';            Size='5.0 GB'; Note='Newest Gemma (Jun 2026). Multimodal, 256K ctx, edge-tuned';
       Repo='unsloth/gemma-4-E4B-it-GGUF';                     File='gemma-4-E4B-it-Q4_K_M.gguf' },

    # --- mid: 16 GB GPUs ----------------------------------------------------
    @{ Tier='mid'; Alias='qwen3.6-27b';   Name='Qwen3.6-27B Q3_K_M';                    Size='13.6 GB'; Note='Newest top dense coder (2026). SWE-bench ~77%. Clean 16 GB fit';
       Repo='unsloth/Qwen3.6-27B-GGUF';                        File='Qwen3.6-27B-Q3_K_M.gguf' },
    @{ Tier='mid'; Alias='qwen3.6-35b';   Name='Qwen3.6-35B-A3B UD-IQ3_S';              Size='13.7 GB'; Note='Newest MoE coder (3B active = fast). Agentic/multi-file work';
       Repo='unsloth/Qwen3.6-35B-A3B-GGUF';                    File='Qwen3.6-35B-A3B-UD-IQ3_S.gguf' },
    @{ Tier='mid'; Alias='gpt-oss-20b';   Name='GPT-OSS-20B (MXFP4)';                   Size='12.1 GB'; Note='OpenAI open-weights. Fast all-round daily driver';
       Repo='ggml-org/gpt-oss-20b-GGUF';                       File='gpt-oss-20b-mxfp4.gguf' },
    @{ Tier='mid'; Alias='devstral';      Name='Devstral-Small-2507 Q4_K_M';            Size='14.3 GB'; Note='Mistral 24B agent/coding model. Strong tool-calling';
       Repo='bartowski/mistralai_Devstral-Small-2507-GGUF';    File='mistralai_Devstral-Small-2507-Q4_K_M.gguf' },
    @{ Tier='mid'; Alias='qwen3-14b';     Name='Qwen3-14B Q5_K_M';                      Size='10.5 GB'; Note='Smaller dense Qwen3. Snappy general/reasoning + tools';
       Repo='bartowski/Qwen_Qwen3-14B-GGUF';                   File='Qwen_Qwen3-14B-Q5_K_M.gguf' },
    @{ Tier='mid'; Alias='mistral-small'; Name='Mistral-Small-3.2-24B-Instruct Q4_K_M'; Size='14.3 GB'; Note='Strong general-purpose + tool use';
       Repo='bartowski/mistralai_Mistral-Small-3.2-24B-Instruct-2506-GGUF';
       File='mistralai_Mistral-Small-3.2-24B-Instruct-2506-Q4_K_M.gguf' },
    @{ Tier='mid'; Alias='gemma4-12b';    Name='Gemma-4-12B-IT Q6_K';                   Size='9.8 GB';  Note='Newest Gemma (Jun 2026). Multimodal text/image/audio, 256K ctx';
       Repo='unsloth/gemma-4-12b-it-GGUF';                     File='gemma-4-12b-it-Q6_K.gguf' },
    @{ Tier='mid'; Alias='phi4';          Name='Phi-4-reasoning-plus (14B) Q5_K_M';     Size='10.5 GB'; Note='Microsoft Phi-4 RL-tuned. Excellent math/logic';
       Repo='bartowski/microsoft_Phi-4-reasoning-plus-GGUF';   File='microsoft_Phi-4-reasoning-plus-Q5_K_M.gguf' },
    @{ Tier='mid'; Alias='deepseek-r1';   Name='DeepSeek-R1-0528-Qwen3-8B Q6_K';        Size='6.5 GB';  Note='R1 reasoning distill at higher quant. Math-heavy';
       Repo='bartowski/deepseek-ai_DeepSeek-R1-0528-Qwen3-8B-GGUF'; File='deepseek-ai_DeepSeek-R1-0528-Qwen3-8B-Q6_K.gguf' },

    # --- large: 24 GB+ GPUs -------------------------------------------------
    @{ Tier='large'; Alias='qwen3.6-27b-q5'; Name='Qwen3.6-27B Q5_K_M';                 Size='19.5 GB'; Note='Top dense coder at high quant. Best quality on 24 GB';
       Repo='unsloth/Qwen3.6-27B-GGUF';                        File='Qwen3.6-27B-Q5_K_M.gguf' },
    @{ Tier='large'; Alias='qwen3.6-35b-q4'; Name='Qwen3.6-35B-A3B UD-Q4_K_M';          Size='22.1 GB'; Note='MoE coder, full Q4. Fast + high quality';
       Repo='unsloth/Qwen3.6-35B-A3B-GGUF';                    File='Qwen3.6-35B-A3B-UD-Q4_K_M.gguf' },
    @{ Tier='large'; Alias='qwen2.5-coder-32b'; Name='Qwen2.5-Coder-32B-Instruct Q5_K_M'; Size='23.0 GB'; Note='SOTA 32B open coder';
       Repo='bartowski/Qwen2.5-Coder-32B-Instruct-GGUF';       File='Qwen2.5-Coder-32B-Instruct-Q5_K_M.gguf' },
    @{ Tier='large'; Alias='qwen3-32b';   Name='Qwen3-32B Q5_K_M';                      Size='23.0 GB'; Note='Frontier dense reasoning model';
       Repo='bartowski/Qwen_Qwen3-32B-GGUF';                   File='Qwen_Qwen3-32B-Q5_K_M.gguf' }
)

# VRAM (MB) -> tier. Used to default the installer's tier selector.
function Get-VramTier {
    param([int]$VramMB)
    if ($VramMB -ge 22000) { return 'large' }
    if ($VramMB -ge 12000) { return 'mid' }
    return 'small'
}
