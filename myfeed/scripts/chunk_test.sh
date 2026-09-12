#!/bin/bash
# 分片上传全流程测试脚本（Git Bash 里跑）
# 用法: ./scripts/chunk_test.sh <登录token> <视频文件路径>
# 依赖: curl / md5sum / split（Git Bash 自带）

set -e

TOKEN=$1
FILE=$2
BASE=http://localhost:8080
CHUNK=5242880   # 5MB，必须和后端约定一致

if [ -z "$TOKEN" ] || [ -z "$FILE" ]; then
  echo "用法: $0 <token> <文件>"; exit 1
fi
if [ ! -f "$FILE" ]; then
  echo "文件不存在: $FILE"; exit 1
fi

FILE_SIZE=$(stat -c%s "$FILE")
FILE_HASH=$(md5sum "$FILE" | cut -d' ' -f1)
TOTAL=$(( (FILE_SIZE + CHUNK - 1) / CHUNK ))
echo "== 文件: $(basename "$FILE") 大小:$FILE_SIZE 分片数:$TOTAL 指纹:$FILE_HASH"

# ① init（第二次跑同一文件时，会返回旧 upload_id + 已传清单 = 断点续传！）
INIT=$(curl -s -X POST "$BASE/video/chunk/init" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"filename\":\"$(basename "$FILE")\",\"file_size\":$FILE_SIZE,\"chunk_size\":$CHUNK,\"total_chunks\":$TOTAL,\"file_hash\":\"$FILE_HASH\"}")
echo "== init 响应: $INIT"

UPLOAD_ID=$(echo "$INIT" | sed -n 's/.*"upload_id":"\([^"]*\)".*/\1/p')
if [ -z "$UPLOAD_ID" ]; then echo "init 失败"; exit 1; fi

# 切片
rm -rf /tmp/chunks && mkdir -p /tmp/chunks
split -b $CHUNK "$FILE" /tmp/chunks/p

# ② 逐片上传（MD5 随片携带，服务端逐片校验）
i=0
for f in /tmp/chunks/p*; do
  CHUNK_HASH=$(md5sum "$f" | cut -d' ' -f1)
  RESP=$(curl -s -X POST "$BASE/video/chunk/upload?upload_id=$UPLOAD_ID&chunk_index=$i&chunk_hash=$CHUNK_HASH" \
    -H "Authorization: Bearer $TOKEN" -F "file=@$f")
  if echo "$RESP" | grep -q "chunk_index"; then
    echo "   片 $i ✓"
  else
    echo "   片 $i ✗ 响应: $RESP"; exit 1
  fi
  i=$((i+1))
done

# ③ status（可选验一次进度）
echo "== status: $(curl -s -X POST "$BASE/video/chunk/status" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"upload_id\":\"$UPLOAD_ID\"}")"

# ④ complete 合并
echo "== complete: $(curl -s -X POST "$BASE/video/chunk/complete" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"upload_id\":\"$UPLOAD_ID\"}")"

rm -rf /tmp/chunks
echo "== 完成。拿 play_url 试试浏览器能不能播放/下载，然后记得 publish！"
