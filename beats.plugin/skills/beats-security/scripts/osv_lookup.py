#!/usr/bin/env python3
"""osv_lookup.py <repo>

Check whether the repo already has known OSV advisories, so the skill can tag
each finding novel / variant-of-known / already-fixed.

Repo-first, NO manifest parsing (no pom.xml / build.gradle):

  1) by git commit — {"commit": <HEAD>} — matches the exact checked-out state
     against any advisory that carries a GIT commit range. Language-agnostic.

  2) by Go module  — {"package":{"ecosystem":"Go","name":<go.mod module>+/vN}} —
     recovers Go advisories that only carry a SEMVER range (no GIT range), which
     a commit query alone would miss (e.g. GHSA-reviewed Go advisories). go.mod's
     `module` line IS the OSV Go package name, so this is a one-line read.

Java / other ecosystems: covered by the commit query whenever an advisory also
carries a GIT range (most CVEs do). SEMVER-only Maven/npm/etc. advisories are not
covered — an accepted gap, in exchange for never parsing pom.xml / build.gradle
(nasty to do reliably across OSS and especially enterprise repos).

Degrades gracefully on network failure. Stdlib only.
"""
import json
import os
import re
import subprocess
import sys
import urllib.request

OSV_URL = "https://api.osv.dev/v1/query"


def git(repo, *args):
    try:
        r = subprocess.run(["git", "-C", repo, *args],
                           capture_output=True, text=True, timeout=10)
        return r.stdout.strip()
    except Exception:
        return ""


def go_module(repo):
    try:
        with open(os.path.join(repo, "go.mod")) as fh:
            for line in fh:
                if line.startswith("module "):
                    return line.split(None, 1)[1].strip()
    except OSError:
        pass
    return ""


def build_queries(repo):
    """Return [(label, osv_payload), ...] — a commit query + Go package queries."""
    qs = []

    commit = git(repo, "rev-parse", "HEAD")
    if re.fullmatch(r"[0-9a-f]{7,40}", commit or ""):
        qs.append((f"commit:{commit[:12]}", {"commit": commit}))

    mod = go_module(repo)
    if mod:
        base = re.sub(r"/v[0-9]+$", "", mod)
        for name in dict.fromkeys([mod, base] + [f"{base}/v{n}" for n in range(2, 10)]):
            qs.append((f"Go:{name}", {"package": {"ecosystem": "Go", "name": name}}))

    return qs


def query(payload, timeout=25):
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        OSV_URL, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def compact(v):
    fixed = []
    for a in v.get("affected", []):
        for rg in a.get("ranges", []):
            for ev in rg.get("events", []):
                if ev.get("fixed"):
                    fixed.append(ev["fixed"])
    return {
        "id": v.get("id"),
        "aliases": v.get("aliases", []),
        "summary": (v.get("summary") or v.get("details", "")[:200] or "")[:200],
        "cwe": v.get("database_specific", {}).get("cwe_ids", []),
        "fixed": sorted(set(fixed)),
    }


def main():
    if len(sys.argv) < 2:
        print("usage: osv_lookup.py <repo>", file=sys.stderr)
        sys.exit(1)
    repo = sys.argv[1]
    queries = build_queries(repo)
    if not queries:
        print(json.dumps({"queried": [], "advisories": [],
                          "error": "no git commit or go.mod to query OSV with"}))
        return

    seen, err = {}, None
    for _label, payload in queries:
        try:
            data = query(payload)
        except Exception as e:  # noqa: BLE001 — any failure degrades to skip
            err = f"{type(e).__name__}: {e}"
            continue
        for v in data.get("vulns", []):
            vid = v.get("id")
            if vid and vid not in seen:
                seen[vid] = compact(v)

    out = {
        "queried": [label for label, _ in queries],
        "advisories": sorted(seen.values(), key=lambda x: x["id"] or ""),
    }
    if err and not seen:
        out["error"] = err  # only surface the error if we got nothing at all
    print(json.dumps(out))


if __name__ == "__main__":
    main()
