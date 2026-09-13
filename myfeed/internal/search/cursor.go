package search

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// maxCursorLen 游标字符串的长度上限（防解码出一个巨大的 JSON）。
// 正常情况下 depth=50、两路各 50 条，融合后最多 100 个 id，
// 序列化再 base64 大概 700 字节 —— 16KB 给了 20 倍余量。
const maxCursorLen = 16 * 1024

// Cursor 搜索结果翻页的游标：**服务端签发的不透明令牌，里面冻着这一轮的完整排名**。
//
// ---------- 为什么不能像其它四条流那样用"镜像游标" ----------
//
// 项目里 /feed/listLatest、listLikesCount、listByPopularity、listByFollowing
// 用的都是同一套手法：ORDER BY 的键就是 WHERE 游标的键，逐个"严格小于"。
// 那套手法有个前提 —— **底层有一个稳定有序的序列可以切片**。
//
// RRF 的输出不是这样的东西：它是每次重新算出来的名次。这次算出来 A 在 B 前面，
// 下次（哪怕语料只多了一条视频）可能就反过来。所以"按分数找位置"这件事
// 在融合结果上根本不成立，不能假装它成立。
//
// ---------- 令牌里冻的是什么 ----------
//
//	IDs    融合后的**完整**有序 id 列表
//	Offset 下一页从第几个开始
//	Mode   本次搜索跑成的形态（翻页时召回没跑，不带上它就报不出去了）
//	Arms   两路各召回多少条（同上）
//
// 第 N 页的处理：解令牌 → 直接切片 → 取详情。**不需要重跑召回，
// 也就不需要重新 embedding 查询** —— 翻页因此比第一页快得多。
//
// ---------- 三个设计点，都值得写下来 ----------
//
//  1. **offset 分页在一般情况下是不安全的**（翻页期间有新数据插入会导致
//     重复或遗漏）。但这里安全，因为**底层顺序被冻住了** ——
//     这正是"排序不可变时 offset 才是对的"这个常见判断的真正边界。
//     顺序可变时用 offset 就是错的，而 RRF 恰好属于"可变"那一类，
//     所以我们先把顺序冻进令牌，让它变成不可变。
//
//  2. **同一轮搜索的翻页是一份快照**：中途新发布的视频不会出现在后续页里。
//     这是**想要的**语义（和 listByPopularity 的 AsOf/NextOffset 快照同源），
//     但和"最新流"的行为不同，得知道自己在用哪一种。
//
//  3. **令牌是客户端持有的，而且不签名。** 改它只能换到另一个**公开视频**的切片
//     （搜索结果本来就人人可见），所以它不是安全边界 ——
//     给它加 HMAC 会让人以为有什么东西需要保护，反而误导。
//     真正要防的是**畸形令牌把服务打挂**：Offset 越界会让切片 panic，
//     所以下面校验得比"安全"需要的更严。
type Cursor struct {
	IDs    []uint `json:"i"`
	Offset int    `json:"o"`
	Mode   Mode   `json:"m"`
	Arms   Arms   `json:"ar"`
}

// EncodeCursor 把游标编成不透明字符串。
//
// base64 用 **RawURLEncoding**（没有 `=` 填充、用 `-`/`_` 而不是 `+`/`/`）：
// 这个字符串会出现在 URL query 里，标准 base64 的 `+` 和 `=` 都需要额外转义，
// 漏转的话表现是"翻页偶尔坏"——取决于 id 里恰好有没有那些字节。
//
// error 在实践中不可达（这几个字段都能序列化），返回它是为了让"Cursor 里
// 加了一个不可序列化字段"这件事在编译/运行期暴露，而不是静默地返回空串 ——
// 空串在调用方那里的含义是"没有下一页"。
func EncodeCursor(c Cursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("序列化搜索游标失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor 解出令牌，并**校验到不可能 panic 的程度**。
//
// 校验存在的理由不是"防坏人"（令牌不签名，改了也没用，见上面的说明），
// 而是"防畸形输入"：客户端可能把上一轮的令牌跨格式复用、可能截断它、
// 也可能手滑传了别的字符串进来。没有下面第一道校验的话，
// 一个 Offset 越界的令牌会让 service 里的切片操作直接 panic —— 500，且带堆栈。
func DecodeCursor(s string) (Cursor, error) {
	if len(s) > maxCursorLen {
		return Cursor{}, fmt.Errorf("游标过长（%d 字节，上限 %d）", len(s), maxCursorLen)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("游标不是合法的 base64url: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("游标不是合法的 JSON: %w", err)
	}
	if c.Offset < 0 || c.Offset > len(c.IDs) {
		// 这一条是**必须**的：Offset 越界 → 切片 panic → 500
		return Cursor{}, fmt.Errorf("游标里的偏移 %d 超出列表长度 %d", c.Offset, len(c.IDs))
	}
	return c, nil
}
