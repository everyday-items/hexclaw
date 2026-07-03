# Baidu realtime hot-search collector — collect-only variant (no KB write).
#
# Same stable extraction path as baidu_hotsearch.star (embedded s-data JSON →
# data.cards[].content[].word — verified against the live page 2026-07-04),
# but WITHOUT the knowledge-base write: a prompt that only asks to collect/
# deliver must not silently persist documents. The numbered list goes into
# data.message, the field deliverResult prefers, so the user receives the
# actual TOP-N list instead of just a title.

UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36"
URL = "https://top.baidu.com/board?tab=realtime"
TOP_N = 20

def collect():
    resp = http_get(URL, headers = {"User-Agent": UA})
    if resp["status"] < 200 or resp["status"] >= 300:
        return {"status": "error", "error": "fetch non-2xx: %d" % resp["status"]}

    blocks = re_findall("(?s)<!--s-data:(.*?)-->", resp["body"])
    if len(blocks) == 0:
        return {"status": "error", "error": "s-data block missing (page structure changed); body head: " + resp["body"][:200]}

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
        # Self-describing failure: carry the actual structure so a heal
        # recompile has evidence to work from instead of guessing again.
        keys = []
        for k in data:
            keys.append(k)
        return {"status": "error", "error": "no hot-search titles extracted; s-data top-level keys: " + ",".join(keys)}

    title = "百度热搜 TOP%d %s" % (TOP_N, now()["date"])
    lines = []
    for i in range(len(titles)):
        lines.append("%d. %s" % (i + 1, titles[i]))
    message = title + "\n" + "\n".join(lines)

    return {"status": "success", "data": {
        "title": title,
        "count": len(titles),
        "message": message,
    }}

emit(collect())
