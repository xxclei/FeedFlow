package search

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// minMatchRunes 走全文索引的最小长度，**必须等于 ngram_token_size**（本机是 2）。
	//
	// 判长度用的是 **rune 数，不是字节数**，这一点必须写死记性：
	// 中文一个字是 3 个字节，`len("三") == 3`。按字节判会把"三"（1 个字）
	// 误送进 MATCH —— 而 ngram 索引里**没有长度 1 的 token**，
	// 于是 MATCH 返回空集、不报错、用户看到"搜不到"。
	// 单字搜索在中国用户手里是最自然的输入之一，所以这条一定要对。
	minMatchRunes = 2

	// maxSearchTokens 一条查询最多保留几个必选词。多出来的丢掉，查询变宽但不报错。
	maxSearchTokens = 8

	// maxTokenRunes 单个 token 的长度上限。防病态输入（用户粘一整段进来）：
	// ngram 会把每个 token 拆成一串 bigram，token 越长拆得越多、
	// 拼出来的短语匹配越贵，而匹配上的概率越低。
	maxTokenRunes = 32

	// likeEscapeChar LIKE 的转义符。选 '!' 而不是默认的 '\'：
	// 默认转义符在 sql_mode 含 NO_BACKSLASH_ESCAPES 时会失效
	// （本机没有这个 flag，但换个环境就有 —— 而"换个环境就有"正是
	// 这类 bug 最难查的形态）。用 '!' 则和 sql_mode 无关。
	likeEscapeChar = "!"
)

// tokenRegex 抽取"字母/数字/下划线"的连续段。
//
// 为什么是**白名单抽取**而不是"转义黑名单"：
// BOOLEAN MODE 的运算符有一大串（+ - > < ( ) ~ * " @ 以及 ft_boolean_syntax
// 里可配置的其它符号）。逐个转义要穷举这张表、还要保证转义之后剩下的东西
// 仍然是合法语法（比如 `+(` 里那个括号转义成什么？）。
// 换成白名单抽取，**运算符从语法上就不可能出现在结果里** —— 这是
// "不构造危险输入"而不是"过滤危险输入"，前者不需要想象力。
//
// 顺带：这个字符类和 tag_entity.go 里 tagRegex 的捕获组**一模一样**，
// 项目里已经有一份直觉了，不用再学一套。
//
// `#` 不在白名单里，所以搜 "#日常" 会得到 token "日常" —— 正是想要的：
// 用户打 # 是在说"这是标签"，而标签在库里存的是**不含 # 的内容**，
// 而描述文本里的 "#日常" 被 ngram 切出来的 token 也是"日常"。
// 两边自然对上，不需要为 # 写任何特判。
var tokenRegex = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// Query 一次词法查询的编译结果。**纯数据、纯函数产出**，没有任何 DB 依赖 ——
// 所以它既可以被 handler 用来判 400，也可以被 service 直接拿去查。
type Query struct {
	// Raw 原始输入，语义那一路要用它去 embed（用户输入的原样文本，
	// 不做 token 化 —— 向量模型自己会分词，而且它比你懂）
	Raw string

	// MatchAnd BOOLEAN MODE 的"全部必须出现"形式：`+红烧肉 +做法`
	MatchAnd string
	// MatchOr 同一批 token 去掉 `+` 的形式：`红烧肉 做法`（出现任一个就有分）
	MatchOr string

	// UseLike 为真表示"不要碰全文索引，走 LIKE"（查询词太短，见 minMatchRunes）
	UseLike bool
	// LikePatterns 走 LIKE 时要**同时满足**（AND）的模式集合，每个都已经转义好
	// 并带上了两侧的 `%`，形如 `%三%`。
	//
	// 转义在这里做而不是在 repo 里：**转义是"查询语法"的一部分**，
	// 和 MatchAnd 的拼装是同一层的事，散到 SQL 那边就会有人漏调。
	//
	// 是一个切片而不是单个模式，因为 LIKE 有**两个**触发点，
	// 而它们要的东西形状不同（理由见下面 CompileQuery 里赋值处）：
	//
	//	UseLike（查询词太短）  → 整个原始输入算一个模式
	//	全文索引零命中的兜底   → 每个 token 一个模式
	LikePatterns []string
}

// CompileQuery 把用户输入编译成可执行的查询。
//
// 返回 *UnsearchableError 表示输入里连一个字母/数字都没有（纯标点/空白）。
// **这个判断必须由调用方翻成 400，不能静默返回空结果** ——
// 空结果的语义是"没有这个视频"，而这里的事实是"这个查询根本没法检索"，
// 前端会因此显示"没有找到相关视频"这种误导性的空态。
//
// 三种输入的走向：
//
//	"vlog 2024"   → MatchAnd `+vlog +2024`
//	"红烧肉"       → MatchAnd `+红烧肉`
//	"三"           → UseLike（1 个字，ngram 索引里没有长度 1 的 token）
//	"C++"          → UseLike（抽出的 token 只有 "C"，也不够 2 个字）
//	"+++ ???"      → UnsearchableError（一个字母数字都没有）
//
// ⚠ 但"token 够长"**不等于**"全文索引一定认得它"。有一条会漏：
// ngram 把含停用词的 bigram 整条丢掉，而默认停用词表里有单字母 a 和 i，
// 于是 "java"（ja/av/va 全含 a）在索引里一个 bigram 都不剩 —— 词再长也搜不到。
// **那种失败在编译期看不出来**（要查索引内容才知道），所以由 lexical_repo.go 的
// 第三档 LIKE 兜底接住。这里只负责把 LikePatterns 备好，不假装能预判。
func CompileQuery(raw string) (Query, error) {
	q := Query{Raw: strings.TrimSpace(raw)}

	tokens := tokenRegex.FindAllString(q.Raw, -1)
	if len(tokens) == 0 {
		return Query{}, &UnsearchableError{Raw: raw}
	}

	usable := make([]string, 0, len(tokens))
	for _, t := range tokens {
		// 不够 ngram_token_size 的 token 既索引不到也搜不到，丢掉。
		// **注意这里是 rune 数**：`len("三")` 是 3，用它判会漏掉这个分支
		if utf8.RuneCountInString(t) < minMatchRunes {
			continue
		}
		if r := []rune(t); len(r) > maxTokenRunes {
			t = string(r[:maxTokenRunes])
		}
		usable = append(usable, t)
		if len(usable) >= maxSearchTokens {
			break
		}
	}

	if len(usable) == 0 {
		// 有字母数字、但都不够长 → LIKE。
		//
		// **刻意不把这种输入当"非法"**：`三`、`C++`、`A` 都是真实的搜索词，
		// 用户搜不到东西是他的事，我们不能替他决定"这个查询不算数"。
		// 代价只是这一路走全表扫描（127 行的表，无所谓；真到了百万行，
		// 该做的是换个 ngram_token_size 或者上真正的搜索引擎，而不是拒绝单字查询）。
		q.UseLike = true
		// **整个原始输入算一个模式**，不是按 token 拆。
		// 这时用户的输入本来就是一个词（"三"、"C++"），拆开只会把 "C++"
		// 变成 "%C%"，语义从"包含 C++ 这个串"漂成"包含字母 C" —— 结果集大 100 倍
		// 而且都不是他要的。
		q.LikePatterns = []string{likePattern(q.Raw)}
		return q, nil
	}

	parts := make([]string, 0, len(usable))
	for _, t := range usable {
		// `+` = 必须出现（AND 语义，比 OR 更符合"搜索框"的预期）
		//
		// **不加 `*` 通配符**：ngram 本身已经把词切成 bigram 了
		// （"红烧肉" → 红烧/烧肉），BOOLEAN 模式下这个 token 会被当成
		// bigram 序列的短语匹配，效果已经是**子串匹配**。
		// 再叠一个 `*` 的交互是版本相关的（有报告说搜索词是句末字符时 `*` 会失效），
		// 先用裸 token，实测漏检再谈 —— search_index.go 的注释里列了对比用的 SQL。
		parts = append(parts, "+"+t)
	}
	q.MatchAnd = strings.Join(parts, " ")

	// OR 形式：同一批 token 去掉 `+`。AND 命中不够时 service 会拿它重试一次。
	// 这就是"搜索引擎"和"严格 AND"的区别：全都要 vs 有就行。
	q.MatchOr = strings.Join(usable, " ")

	// **每个 token 一个模式**（和上面 UseLike 那条相反，理由也在那边）。
	// 这条只在"全文索引连一条都没命中"时才会被用到（见 lexical_repo.go 的第三档）。
	//
	// 为什么按 token 而不是按整串：走到那一档时用户输的是一个短语（"java 教程"），
	// 而整串 LIKE 要求标题/描述里有"java 教程"这个**连着的**子串 ——
	// 库里那条叫 "java-...教程" 的会因此漏掉。按 token AND 才是"用户要的意思"
	// 最直接的翻译，也是 MatchAnd 语义的逐字对应。
	q.LikePatterns = make([]string, 0, len(usable))
	for _, t := range usable {
		q.LikePatterns = append(q.LikePatterns, likePattern(t))
	}
	return q, nil
}

// likePattern 把一个**可能含 LIKE 元字符**的串包成"包含它"的模式。
//
// 包 `%` 和转义都在这里做，调用方不用记两件事 —— 上面两处赋值都不自己拼 `%`。
func likePattern(s string) string { return "%" + escapeLike(s) + "%" }

// likeEscaper 转义 LIKE 的元字符。
//
// **顺序要紧**：'!' 必须第一个换，否则会把后面补出来的 '!' 再转一遍
// （`strings.NewReplacer` 是一次扫描、不回头，所以这里其实是安全的；
// 但换成链式 Replace 就会错 —— 记这条是因为它很容易被"优化"成链式）。
var likeEscaper = strings.NewReplacer(
	likeEscapeChar, likeEscapeChar+likeEscapeChar,
	"%", likeEscapeChar+"%",
	"_", likeEscapeChar+"_",
)

func escapeLike(s string) string { return likeEscaper.Replace(s) }
