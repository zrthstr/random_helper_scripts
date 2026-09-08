# AWS_IMDSv1_enum.py - a simple recusive AWS IMDSv1 crawler
#
# MIT License
# Copyright (c) 2022, zrthstr
#
# AWS IMDSv1 has a few quirks.
# For some reason, unlike all other dir-entries from /latest/ do not end in a slash.
#
# TODO: Accommodate for some more irregularities in IMDS, for example:
# * curl http://169.254.169.254/latest/meta-data/public-keys/ -> 0=LightsailDefaultKeyPair
#   But then we need to continue with /latest/meta-data/public-keys/0, not 0=LightsailDefaultKeyPair.
# * What else?


import requests

first = ['meta-data', 'dynamic', 'user-data', 'security-groups']
base_url = "http://169.254.169.254/latest/"

def recurse(base):
    for r in requests.get(base).text.splitlines():
        if r.endswith("/"):
            recurse(base+r)
        else:
            response = requests.get(base + r)
            if response.status_code != 200:
                continue
            f = response.text
            ret[base+r]=f

def print_sorted_by_alpha(ret):
    for key, value in dict(sorted(ret.items())).items():
        print(f"{key}:\n => {value}")

for f in first:
    ret = {}
    print(f"[+] Trying {f}")
    recurse(base_url + f + "/")
    print_sorted_by_alpha(ret)
