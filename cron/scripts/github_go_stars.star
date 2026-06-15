# GitHub golang/go star-count collector — Starlark (pure-Go engine, zero deps).
#
# Deterministic, zero-LLM, no python required. Fetches the repo JSON from the
# GitHub API, reads stargazers_count, and writes a one-line digest into the local
# knowledge base via the in-process kb_ingest builtin (loopback http_post is
# SSRF-blocked). Builtins (http_get/json_decode/now/kb_ingest/emit) are injected
# by StarlarkEngine. GitHub's API requires a User-Agent header.

UA = "hexclaw-cron/1.0 (+https://github.com/hexagon-codes/hexclaw)"
URL = "https://api.github.com/repos/golang/go"

def collect():
    resp = http_get(URL, headers = {"User-Agent": UA, "Accept": "application/vnd.github+json"})
    if resp["status"] < 200 or resp["status"] >= 300:
        return {"status": "error", "error": "fetch non-2xx: %d" % resp["status"]}

    data = json_decode(resp["body"])
    stars = data.get("stargazers_count", -1)
    if stars < 0:
        return {"status": "error", "error": "stargazers_count missing (API response changed)"}

    today = now()["date"]
    doc_title = "golang-go-stars " + today
    content = "golang/go 截至 %s 的 GitHub star 数为 %d。" % (today, stars)

    kb_ingest(title = doc_title, content = content, source = "cron-gh-stars")

    return {"status": "success", "data": {
        "title": doc_title,
        "stars": stars,
        "message": "%s · %d stars" % (doc_title, stars),
    }}

emit(collect())
