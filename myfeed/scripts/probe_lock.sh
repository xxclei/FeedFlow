#!/bin/bash
# 阶段7 验证：防击穿锁到底挡掉了多少条 SQL。
#
# 为什么需要这个脚本而不是手敲：这个实验要跑 6 组（有锁/无锁 × 三档 DB 延迟），
# 每组都必须"重启服务 → 删缓存 → 立刻打并发"。任何一步手抖都会得到假结论 ——
# 尤其是**忘了删缓存**：那样 200 个请求全命中缓存，你会看到 0 条 SQL，
# 看起来"锁完美生效"，其实那个 0 和锁毫无关系。
#
# 用法: probe_lock.sh <DB延迟ms> <拆锁0|1> <标签>
#
# ⚠ 这个脚本依赖两个**已经拆掉的**探针，直接跑没用：
#   PROBE_DB_DELAY_MS  在 internal/db/ 里注册一个 GORM Query 回调，把每条 SELECT 拖慢
#   PROBE_NO_LOCK      在 video_service.go 里给锁 key 加纳秒后缀，让每个请求都"抢到锁"
# 要重跑实验，得先把这两个探针加回去（改动和当时的实验结果见 git 历史/对话记录）。
# 留在这里是为了记住**实验设计本身**，而不只是结论。
set -u
cd "$(dirname "$0")/.." || exit 1

DELAY="${1:-0}"
NOLOCK="${2:-0}"
LABEL="${3:-unknown}"
HEY=/c/Users/46392/go/bin/hey.exe
VIDEO_ID=279
CACHE_KEY="v1:video:detail:id=${VIDEO_ID}"

redis() { docker exec myfeed-redis redis-cli -a 123456 --no-auth-warning "$@" 2>/dev/null; }
sqlnow() { docker exec myfeed-mysql mysql -uroot -p123456 -N -B \
  -e "show global status like 'Com_stmt_execute'" 2>/dev/null | awk '{print $2}'; }

# ---------- 1. 用指定的环境变量重启服务 ----------
powershell -NoProfile -Command \
  "Get-Process myfeed -ErrorAction SilentlyContinue | Stop-Process -Force" >/dev/null 2>&1
sleep 1
PROBE_DB_DELAY_MS="$DELAY" PROBE_NO_LOCK="$NOLOCK" powershell -NoProfile -Command \
  "Start-Process -FilePath '.run\myfeed.exe' -WorkingDirectory (Get-Location) \
   -RedirectStandardOutput '.run\server.log' -RedirectStandardError '.run\server.err.log' \
   -WindowStyle Hidden" >/dev/null 2>&1
sleep 3

if ! curl -s -m 5 http://127.0.0.1:8080/healthz | grep -q ok; then
  echo "[$LABEL] 服务没起来，中止"; exit 1
fi

# ---------- 2. 删缓存（这一步不能省，见文件头）----------
redis DEL "$CACHE_KEY" >/dev/null
if [ "$(redis EXISTS "$CACHE_KEY")" != "0" ]; then
  echo "[$LABEL] 缓存没删掉，中止"; exit 1
fi

# ---------- 3. 打并发 ----------
A=$(sqlnow)
$HEY -n 200 -c 200 -m POST -H "Content-Type: application/json" \
     -d "{\"id\":${VIDEO_ID}}" "http://127.0.0.1:8080/video/getDetail" \
     > "/tmp/hey_${LABEL}.txt" 2>&1
B=$(sqlnow)

# ---------- 4. 汇总 ----------
avg=$(grep -E "^  Average:" "/tmp/hey_${LABEL}.txt" | awk '{print $2}')
slow=$(grep -E "^  Slowest:" "/tmp/hey_${LABEL}.txt" | awk '{print $2}')
ok200=$(grep -E "^\s+\[200\]" "/tmp/hey_${LABEL}.txt" | awk '{print $2}')
notok=$(grep -E "^\s+\[(4|5)" "/tmp/hey_${LABEL}.txt" | tr -d ' \n')

printf "%-18s delay=%-5s 拆锁=%-2s | SQL 增量=%-4s | 200=%s %s | avg=%ss slowest=%ss\n" \
  "$LABEL" "$DELAY" "$NOLOCK" "$((B-A))" "${ok200:-0}" "${notok:+- 非200:$notok}" "$avg" "$slow"
