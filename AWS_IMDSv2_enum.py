#!/usr/bin/env python3
import requests, os, base64

IMDS_HOST = "169.254.169.254"
BASE = f"http://{IMDS_HOST}/latest/"
HEADERS = {}
OUTDIR = "imds_dump"

def safe_filename(path):
    return path.strip("/").replace("/", "___") or "root"

def save(path, content):
    os.makedirs(OUTDIR, exist_ok=True)
    fname = os.path.join(OUTDIR, safe_filename(path))
    with open(fname, "wb") as f:
        if isinstance(content, str):
            f.write(content.encode())
        else:
            f.write(content)

def get_token(ttl=21600):
    r = requests.put(
        BASE + "api/token",
        headers={"X-aws-ec2-metadata-token-ttl-seconds": str(ttl)},
        timeout=2,
    )
    r.raise_for_status()
    return r.text

def get(path):
    r = requests.get(BASE + path, headers=HEADERS, timeout=3)
    if r.status_code == 404:
        return None
    r.raise_for_status()
    return r

def is_dir_listing(resp):
    return resp.headers.get("Content-Type", "").startswith("text/plain") and "\n" in resp.text

def crawl(path):
    r = get(path)
    if r is None:
        return
    if path.endswith("/") and is_dir_listing(r):
        for item in filter(None, (x.strip() for x in r.text.splitlines())):
            child_path = path + item
            if item.endswith("/") and not child_path.endswith("/"):
                child_path += "/"
            crawl(child_path)
    else:
        try:
            save(path, r.text)
        except UnicodeDecodeError:
            save(path, r.content)

def main():
    global HEADERS
    token = get_token()
    HEADERS = {"X-aws-ec2-metadata-token": token}
    for root in ["meta-data/", "dynamic/", "user-data"]:
        crawl(root)

if __name__ == "__main__":
    main()

