import requests

base_url = "http://169.254.169.254/latest/meta-data/"
ret = {}

def next(base):
    for r in requests.get(base).text.splitlines():
        if r.endswith("/"):
            next(base+r)
        else:
            f = requests.get(base + r).text
            ret[base+r]=f

next(base_url)

for key, value in dict(sorted(ret.items())).items():
    print(f"{key}:\n => {value}")
