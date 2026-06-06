#!/usr/bin/env python3
# Start the LiteLLM proxy via its installed console-script entry point, using the
# venv's python directly. This avoids two Windows pitfalls:
#   * the litellm.exe shim hardcodes the venv path and breaks if the folder moves
#   * `python -m litellm` fails - litellm ships no __main__ module
# Args after the script name are passed straight through to the litellm CLI, e.g.
#   python litellm_run.py --config run.yaml --port 4000 --host 127.0.0.1
import sys
from importlib import metadata


def find_entry():
    eps = metadata.entry_points()
    try:
        cands = list(eps.select(group="console_scripts", name="litellm"))
    except AttributeError:  # Python 3.9 returns a dict
        cands = [e for e in eps.get("console_scripts", []) if e.name == "litellm"]
    return cands[0] if cands else None


def main():
    # LiteLLM prints a Unicode banner via click.echo at startup. When stdout is
    # redirected to a file, Windows defaults to cp1252 and the banner triggers a
    # UnicodeEncodeError that aborts startup. Force UTF-8 on our streams.
    for s in (sys.stdout, sys.stderr):
        try:
            s.reconfigure(encoding="utf-8", errors="replace")
        except Exception:
            pass
    ep = find_entry()
    if ep is None:
        sys.stderr.write("[x] litellm console-script entry point not found in this venv\n")
        return 1
    func = ep.load()           # e.g. litellm:run_server
    sys.argv = ["litellm"] + sys.argv[1:]
    func()                      # click command reads sys.argv
    return 0


if __name__ == "__main__":
    sys.exit(main())
