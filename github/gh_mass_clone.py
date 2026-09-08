#!/usr/bin/env python3
"""
gh_mass_clone.py - clone (or update) every repository of a GitHub user or org.

MIT License
Copyright (c) 2026, zrthstr

Auth: set GITHUB_TOKEN (or GH_TOKEN), or pass --token-file.
Deliberately does NOT take --token on the command line: argv is world-readable
via /proc, so a token there leaks to every user on the box.

The token is handed to git through GIT_CONFIG_* env vars as an http.extraheader,
the same trick actions/checkout uses. That means:
  * the token never gets written into .git/config
  * the remote URL stays a clean https://github.com/owner/repo.git
  * the token never shows up in `ps`
Compare to the usual https://<token>@github.com/... approach, which persists the
token in every clone's .git/config and follows it into backups and screenshares.

Examples:
    ./gh_mass_clone.py torvalds
    ./gh_mass_clone.py --dest ~/backup --mirror some-org
    ./gh_mass_clone.py --no-forks --no-archived --jobs 8 some-org
    ./gh_mass_clone.py --list-only some-org
"""

import argparse
import base64
import concurrent.futures
import os
import subprocess
import sys
import time

import requests

API = "https://api.github.com"
UA = {"User-Agent": "gh_mass_clone", "Accept": "application/vnd.github+json"}


def die(msg):
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(1)


def get_token(token_file):
    if token_file:
        with open(os.path.expanduser(token_file)) as f:
            return f.read().strip()
    for var in ("GITHUB_TOKEN", "GH_TOKEN"):
        if os.environ.get(var):
            return os.environ[var].strip()
    # fall back to whatever the gh cli already has
    try:
        out = subprocess.run(["gh", "auth", "token"], capture_output=True, text=True, timeout=10)
        if out.returncode == 0 and out.stdout.strip():
            return out.stdout.strip()
    except (FileNotFoundError, subprocess.TimeoutExpired):
        pass
    return None


def api_session(token):
    s = requests.Session()
    s.headers.update(UA)
    if token:
        s.headers["Authorization"] = f"Bearer {token}"
    return s


def paginate(session, url, params=None):
    """
    Walk the Link: rel=next chain and return every item.

    Returns None (not []) if the endpoint 404s, so callers can tell
    "this owner is not an org" apart from "this org has no repos".
    """
    params = dict(params or {})
    params.setdefault("per_page", 100)
    items = []
    while url:
        r = session.get(url, params=params, timeout=30)
        params = None  # subsequent URLs already carry the query string
        if r.status_code == 403 and r.headers.get("X-RateLimit-Remaining") == "0":
            reset = int(r.headers.get("X-RateLimit-Reset", 0))
            wait = max(0, reset - int(time.time())) + 2
            print(f"rate limited, sleeping {wait}s", file=sys.stderr)
            time.sleep(wait)
            continue
        if r.status_code == 404:
            return None
        r.raise_for_status()
        items.extend(r.json())
        url = r.links.get("next", {}).get("url")
    return items


def whoami(session):
    r = session.get(f"{API}/user", timeout=30)
    return r.json().get("login") if r.status_code == 200 else None


def list_repos(session, owner, token):
    """
    Three different endpoints, because GitHub treats them differently:
      /user/repos    - the only one that returns YOUR OWN private repos
      /orgs/X/repos  - org repos, private ones included if the token can see them
      /users/X/repos - public repos only, always, even with a token
    """
    if token and owner.lower() == (whoami(session) or "").lower():
        print(f"[*] {owner} is the authenticated user, using /user/repos (includes private)")
        return paginate(session, f"{API}/user/repos", {"affiliation": "owner", "type": "all"})

    repos = paginate(session, f"{API}/orgs/{owner}/repos", {"type": "all"})
    if repos is not None:
        print(f"[*] {owner} resolved as an org")
        return repos

    repos = paginate(session, f"{API}/users/{owner}/repos", {"type": "owner"})
    if repos is None:
        die(f"no such user or org: {owner}")
    print(f"[*] {owner} resolved as a user (public repos only)")
    return repos


def git_env(token):
    """Inject credentials via env so they never touch disk or argv."""
    env = os.environ.copy()
    if token:
        basic = base64.b64encode(f"x-access-token:{token}".encode()).decode()
        env["GIT_CONFIG_COUNT"] = "1"
        env["GIT_CONFIG_KEY_0"] = "http.https://github.com/.extraheader"
        env["GIT_CONFIG_VALUE_0"] = f"Authorization: Basic {basic}"
    env["GIT_TERMINAL_PROMPT"] = "0"  # fail instead of hanging on a password prompt
    return env


def run_git(args, env, cwd=None):
    return subprocess.run(
        ["git"] + args, env=env, cwd=cwd,
        capture_output=True, text=True,
    )


def sync_one(repo, dest, env, use_ssh, mirror, shallow, dry_run):
    name = repo["name"]
    url = repo["ssh_url"] if use_ssh else repo["clone_url"]
    path = os.path.join(dest, name + (".git" if mirror else ""))

    if dry_run:
        return name, "would clone", url

    if os.path.isdir(path):
        if mirror:
            r = run_git(["remote", "update", "--prune"], env, cwd=path)
        else:
            r = run_git(["fetch", "--all", "--prune", "--tags"], env, cwd=path)
        if r.returncode != 0:
            return name, "fetch failed", r.stderr.strip().splitlines()[-1:] or ""
        # only fast-forward; never clobber local work
        if not mirror:
            run_git(["merge", "--ff-only", "@{u}"], env, cwd=path)
        return name, "updated", ""

    args = ["clone"]
    if mirror:
        args.append("--mirror")
    elif shallow:
        args += ["--depth", "1"]
    args += [url, path]
    r = run_git(args, env)
    if r.returncode != 0:
        return name, "clone failed", (r.stderr.strip().splitlines() or [""])[-1]
    return name, "cloned", ""


def main():
    p = argparse.ArgumentParser(description="Mass clone/update all repos of a GitHub user or org.")
    p.add_argument("owner", help="GitHub user or org name")
    p.add_argument("--dest", default=".", help="destination dir (default: cwd); repos land in <dest>/<owner>/")
    p.add_argument("--token-file", help="read token from this file instead of the environment")
    p.add_argument("--ssh", action="store_true", help="clone over SSH instead of HTTPS+token")
    p.add_argument("--mirror", action="store_true", help="bare --mirror clones (what you want for backups)")
    p.add_argument("--shallow", action="store_true", help="--depth 1 (ignored with --mirror)")
    p.add_argument("--no-forks", action="store_true", help="skip forks")
    p.add_argument("--no-archived", action="store_true", help="skip archived repos")
    p.add_argument("--jobs", type=int, default=4, help="parallel git processes (default 4)")
    p.add_argument("--list-only", action="store_true", help="print the repo list and exit")
    p.add_argument("--dry-run", action="store_true", help="show what would happen, clone nothing")
    args = p.parse_args()

    token = get_token(args.token_file)
    if not token:
        print("[!] no token found; only public repos will be visible", file=sys.stderr)

    session = api_session(token)
    repos = list_repos(session, args.owner, token)

    if args.no_forks:
        repos = [r for r in repos if not r.get("fork")]
    if args.no_archived:
        repos = [r for r in repos if not r.get("archived")]
    repos.sort(key=lambda r: r["name"].lower())

    print(f"[*] {len(repos)} repos")
    if args.list_only:
        for r in repos:
            flags = ",".join(f for f in ("private" if r.get("private") else "",
                                         "fork" if r.get("fork") else "",
                                         "archived" if r.get("archived") else "") if f)
            print(f"  {r['full_name']}{' [' + flags + ']' if flags else ''}")
        return

    dest = os.path.join(os.path.expanduser(args.dest), args.owner)
    os.makedirs(dest, exist_ok=True)
    env = git_env(None if args.ssh else token)

    # jobs stays low by default: GitHub throttles bursts of clones (secondary rate limit)
    ok = failed = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.jobs) as pool:
        futures = [
            pool.submit(sync_one, r, dest, env, args.ssh, args.mirror, args.shallow, args.dry_run)
            for r in repos
        ]
        for fut in concurrent.futures.as_completed(futures):
            name, status, detail = fut.result()
            print(f"  {status:>12}  {name}  {detail}")
            if "failed" in status:
                failed += 1
            else:
                ok += 1

    print(f"[*] done: {ok} ok, {failed} failed -> {dest}")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
