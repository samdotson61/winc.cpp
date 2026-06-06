#!/usr/bin/env python3
# Download one file from a HuggingFace repo with a clean 1-second progress bar.
# Used by winc.cpp (install.ps1 and winc.ps1). Calls the venv python directly,
# so it never depends on the hf.exe console shim (which breaks on folder rename).
#
#   python hf_get.py <repo_id> <filename> <local_dir>
#
# Reads HF_TOKEN from the environment for gated repos. httpx (a huggingface_hub
# dependency) strips the auth header on cross-host CDN redirects automatically.
import os
import sys
import time


def fmt_time(s):
    s = int(s)
    h, m, sec = s // 3600, (s % 3600) // 60, s % 60
    return ("%d:%02d:%02d" % (h, m, sec)) if h else ("%02d:%02d" % (m, sec))


def gb(b):
    return b / 1_000_000_000.0


def fallback(repo, filename, local_dir, token):
    # Robust path: let huggingface_hub handle it (Xet, resume, auth) with its bar.
    from huggingface_hub import hf_hub_download
    hf_hub_download(repo_id=repo, filename=filename, local_dir=local_dir, token=token)


def main():
    if len(sys.argv) < 4:
        print("usage: hf_get.py <repo> <file> <local_dir>", file=sys.stderr)
        return 2
    repo, filename, local_dir = sys.argv[1], sys.argv[2], sys.argv[3]
    os.makedirs(local_dir, exist_ok=True)
    dest = os.path.join(local_dir, os.path.basename(filename))
    part = dest + ".part"
    token = os.environ.get("HF_TOKEN") or None

    # Resolve the download URL + headers (prefer the library's helpers).
    try:
        from huggingface_hub import hf_hub_url
        from huggingface_hub.utils import build_hf_headers
        url = hf_hub_url(repo_id=repo, filename=filename)
        headers = build_hf_headers(token=token)
    except Exception:
        endpoint = os.environ.get("HF_ENDPOINT", "https://huggingface.co")
        url = "%s/%s/resolve/main/%s" % (endpoint, repo, filename)
        headers = {"User-Agent": "winc.cpp"}
        if token:
            headers["Authorization"] = "Bearer " + token

    try:
        import httpx
    except Exception:
        fallback(repo, filename, local_dir, token)
        return 0

    try:
        with httpx.stream("GET", url, headers=headers, follow_redirects=True,
                          timeout=httpx.Timeout(30.0, read=None)) as r:
            r.raise_for_status()
            total = int(r.headers.get("Content-Length") or 0)
            done = 0
            start = time.monotonic()
            last = 0.0
            barw = 28
            with open(part, "wb") as f:
                for chunk in r.iter_bytes(1024 * 1024):
                    f.write(chunk)
                    done += len(chunk)
                    now = time.monotonic()
                    if now - last >= 1.0 or (total and done >= total):
                        last = now
                        el = now - start
                        spd = done / el if el > 0 else 0.0
                        if total:
                            pct = done / total
                            fill = int(barw * pct)
                            bar = "#" * fill + "-" * (barw - fill)
                            eta = (total - done) / spd if spd > 0 else 0
                            sys.stdout.write(
                                "\r  [%s] %3d%%  %.2f/%.2f GB  %.1f MB/s  ETA %s   "
                                % (bar, int(pct * 100), gb(done), gb(total), spd / 1e6, fmt_time(eta)))
                        else:
                            sys.stdout.write("\r  %.2f GB  %.1f MB/s   " % (gb(done), spd / 1e6))
                        sys.stdout.flush()
            sys.stdout.write("\n")
            sys.stdout.flush()
        os.replace(part, dest)
        return 0
    except Exception as e:
        try:
            if os.path.exists(part):
                os.remove(part)
        except Exception:
            pass
        print("\n[!] direct download failed (%s); retrying via huggingface_hub..." % e, file=sys.stderr)
        try:
            fallback(repo, filename, local_dir, token)
            return 0
        except Exception as e2:
            print("[x] download failed: %s" % e2, file=sys.stderr)
            return 1


if __name__ == "__main__":
    sys.exit(main())
