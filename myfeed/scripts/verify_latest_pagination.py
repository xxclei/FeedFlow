"""Walk /feed/listLatest page by page and prove the cursor never repeats or skips.

Why this exists: the cold/hot split in ListLatest has three separate ways to
produce a wrong page, and none of them shows up in a smoke test.

  1. The ZSET branch returns ids in descending score order; GetVideoByIDs
     re-orders by input, so order is fine -- but if buildOrderedResult were
     missing, Go's randomised map iteration would shuffle every page.
  2. `maxScore = 游标 - 1` (ZSET, inclusive bound) must mean the same thing as
     `create_time < 游标` (SQL, exclusive bound).  Off by one -> the last video
     of page N is the first video of page N+1.
  3. The cold/hot boundary stitch uses the last *surviving* hot video's
     create_time as the cold cursor.  A deleted video, or an exhausted hot
     region, makes that boundary move.

A wrong page still returns HTTP 200 with plausible-looking videos, so the only
way to catch it is to concatenate every page and diff against the DB's own
ordering.  Run it, do not eyeball it.

Usage:
    python scripts/verify_latest_pagination.py [limit]
"""

import json
import subprocess
import sys
import urllib.request

BASE = "http://127.0.0.1:8080/feed/listLatest"
TOKEN = (
    "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9."
    "eyJhY2NvdW50X2lkIjoxLCJ1c2VybmFtZSI6ImFmIiwiZXhwIjoxNzg5NDQ5Nzc3LCJuYmYiOjE3ODkzNjMzNzcsImlhdCI6MTc4OTM2MzM3N30."
    "4JX3tt-Z9QozlC93aEKd_l8uwItVIOjNlBofzR1rdVU"
)


def db_order():
    """The ground truth: every video id, newest first."""
    out = subprocess.run(
        ["docker", "exec", "myfeed-mysql", "mysql", "-uroot", "-p123456", "-N", "-e",
         "select id from myfeed.videos order by create_time desc"],
        capture_output=True, text=True,
    ).stdout
    return [int(x) for x in out.split()]


def walk(limit):
    """Page until has_more is false. Returns (ids, per_page_report)."""
    ids, report = [], []
    cursor = 0
    for page in range(1, 200):
        body = {"limit": limit}
        if cursor:
            # The field is `latest_time`, NOT `before`.  Getting this wrong is
            # silent: gin ignores unknown JSON keys, so every request is treated
            # as page 1 and next_time never changes.  The loop guard below is
            # what turns that into a loud failure instead of an infinite loop.
            body["latest_time"] = cursor
        req = urllib.request.Request(
            BASE,
            data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json",
                     "Authorization": "Bearer " + TOKEN},
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.load(resp)

        page_ids = [v["id"] for v in data["video_list"]]
        ids.extend(page_ids)
        report.append((page, len(page_ids), data["next_time"], data["has_more"]))

        if not data["has_more"]:
            break
        if data["next_time"] == cursor:
            report.append((page, -1, data["next_time"], None))
            raise SystemExit("FATAL: cursor did not advance -- page %d would loop forever" % page)
        cursor = data["next_time"]
    return ids, report


def check(label, limit):
    got, report = walk(limit)
    want = db_order()

    print("  %s  limit=%d  pages=%d  ids=%d" % (label, limit, len(report), len(got)))

    dupes = [x for x in set(got) if got.count(x) > 1]
    ok = True
    if dupes:
        print("    FAIL duplicates: %s" % sorted(dupes))
        ok = False
    if got != want:
        missing = [x for x in want if x not in got]
        extra = [x for x in got if x not in want]
        print("    FAIL sequence mismatch vs DB order")
        if missing:
            print("      missing (%d): %s" % (len(missing), missing[:10]))
        if extra:
            print("      extra   (%d): %s" % (len(extra), extra[:10]))
        for i, (a, b) in enumerate(zip(got, want)):
            if a != b:
                print("      first divergence at index %d: got %s want %s" % (i, a, b))
                break
        ok = False
    if ok:
        print("    OK  %d ids, no dupes, order identical to the DB" % len(got))
    return ok


def main():
    limit = int(sys.argv[1]) if len(sys.argv) > 1 else 10
    print("walking /feed/listLatest (limit=%d)" % limit)
    return 0 if check("pagination", limit) else 1


if __name__ == "__main__":
    sys.exit(main())
