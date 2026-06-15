# Baidu realtime hot-search collector — Starlark (pure-Go engine, zero deps).
#
# Deterministic, zero-LLM, no python required. Parses the page's embedded s-data
# JSON (stable, unlike the c-single-text-ellipsis CSS class) and upserts the
# TOP 20 titles into the local knowledge base via the in-process kb_ingest
# builtin (loopback http_post is SSRF-blocked). Builtins (http_get/json_decode/
# json_encode/re_findall/now/kb_ingest/emit) are injected by StarlarkEngine.

UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36"
URL = "https://top.baidu.com/board?tab=realtime"
TOP_N = 20

def collect():
    resp = http_get(URL, headers = {"User-Agent": UA})
    if resp["status"] < 200 or resp["status"] >= 300:
        return {"status": "error", "error": "fetch non-2xx: %d" % resp["status"]}

    blocks = re_findall("(?s)<!--s-data:(.*?)-->", resp["body"])
    if len(blocks) == 0:
        return {"status": "error", "error": "s-data block missing (page structure changed)"}

    data = json_decode(blocks[0])
    titles = []
    seen = {}
    for card in data.get("data", {}).get("cards", []):
        for item in card.get("content", []):
            word = item.get("word", "").strip()
            if word and word not in seen:
                seen[word] = True
                titles.append(word)
                if len(titles) >= TOP_N:
                    break
        if len(titles) >= TOP_N:
            break

    if len(titles) == 0:
        return {"status": "error", "error": "no hot-search titles extracted"}

    doc_title = "百度热搜 " + now()["date"]
    lines = []
    for i in range(len(titles)):
        lines.append("%d. %s" % (i + 1, titles[i]))
    content = "\n".join(lines)

    kb_ingest(title = doc_title, content = content, source = "cron-baidu-hotsearch")

    return {"status": "success", "data": {"title": doc_title, "count": len(titles)}}

emit(collect())
