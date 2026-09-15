"""极简 mp4 探针：不依赖 ffprobe，直接解析 moov/mvhd 原子拿时长。

用途：算出素材的真实码率 = 文件大小 * 8 / 时长。
背景：项目里没有 ffmpeg/ffprobe（`which ffprobe` 是空的），
     但 MP4 的时长就写在 moov->mvhd 里，几十行就能读出来。

MP4 结构（只关心两层）：
    [4B size][4B type] payload ...        <- 顶层 box 链
    moov 里面装着 mvhd，mvhd 里有 timescale 和 duration
    真实秒数 = duration / timescale

size 字段有两种特殊值：
    1  = 真的 size 在后面 8 字节里（64 位）
    0 = 这个 box 一直到文件末尾（只在最后一个 box 出现）
"""
import os
import struct
import sys


def iter_boxes(f, end):
    """从当前位置到 end 逐个遍历 box，yield (box_type, payload_offset, box_size)"""
    while f.tell() < end:
        pos = f.tell()
        hdr = f.read(8)
        if len(hdr) < 8:
            return
        size, btype = struct.unpack(">I4s", hdr)
        if size == 1:  # 64 位长度
            size = struct.unpack(">Q", f.read(8))[0]
        elif size == 0:  # 直到文件尾
            size = end - pos
        if size < 8 or pos + size > end:
            return
        yield btype, pos + 8, size
        f.seek(pos + size)


def find_box(f, start, end, target):
    """在 [start,end) 里找某个 box，返回 (payload_offset, box_size)"""
    f.seek(start)
    for btype, off, size in iter_boxes(f, end):
        if btype == target:
            return off, size
    return None, None


def mp4_duration(path):
    """返回秒数，失败返回 None"""
    fsize = os.path.getsize(path)
    with open(path, "rb") as f:
        moov_off, moov_size = find_box(f, 0, fsize, b"moov")
        if moov_off is None:
            return None
        # mvhd 在 moov 的 payload 里；moov 内部的 box 起点是 moov_off
        mvhd_off, _ = find_box(f, moov_off, moov_off + moov_size - 8, b"mvhd")
        if mvhd_off is None:
            return None

        f.seek(mvhd_off)
        raw = f.read(32)
        version = raw[0]
        if version == 1:
            timescale, duration = struct.unpack(">IQ", raw[20:32])
        else:
            timescale, duration = struct.unpack(">II", raw[12:20])
        if not timescale:
            return None
        return duration / timescale


def human(sec):
    m, s = divmod(int(sec), 60)
    return f"{m}m{s:02d}s"


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else ".run/uploads/videos"
    files = []
    for dirpath, _, names in os.walk(root):
        for n in names:
            if n.lower().endswith(".mp4"):
                p = os.path.join(dirpath, n)
                files.append((os.path.getsize(p), p))

    files.sort(reverse=True)
    print(f"found {len(files)} mp4 files under {root}\n")

    rows = []
    for size, p in files:
        d = mp4_duration(p)
        if d and d > 0:
            mb = size / 1048576
            mbps = size * 8 / d / 1_000_000
            rows.append((mbps, mb, d, p))

    if not rows:
        print("no parsable mp4 (all failed)")
        return

    rows.sort(reverse=True)
    print(f"{'bitrate':>9} {'size':>8} {'dur':>7}   file")
    print("-" * 72)
    for mbps, mb, d, p in rows[:12]:
        print(f"{mbps:8.2f}M {mb:7.1f}MB {human(d):>7}   {os.path.basename(p)}")
    print("...")
    for mbps, mb, d, p in rows[-4:]:
        print(f"{mbps:8.2f}M {mb:7.1f}MB {human(d):>7}   {os.path.basename(p)}")

    n = len(rows)
    avg_mbps = sum(r[0] for r in rows) / n
    avg_mb = sum(r[1] for r in rows) / n
    avg_d = sum(r[2] for r in rows) / n
    print()
    print(f"n={n}  avg bitrate={avg_mbps:.2f} Mbps  avg size={avg_mb:.1f} MB  avg dur={human(avg_d)}")
    print(f"median bitrate={sorted(r[0] for r in rows)[n//2]:.2f} Mbps")
    print(f"total size={sum(r[1] for r in rows):.0f} MB")


if __name__ == "__main__":
    main()
