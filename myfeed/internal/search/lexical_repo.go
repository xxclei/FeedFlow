package search

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"myfeed/internal/video"
)

// ErrNoFullTextIndex 词法那一路不可用：videos 表上没有能匹配 (title, description)
// 的 FULLTEXT 索引（MySQL 1191）。
//
// 这是一个**可降级**的信号，不是故障：调用方收到它应该退成纯向量搜索，
// 而不是把 500 抛给用户。它同时解释了为什么 search_index.go 敢在建索引失败时
// 只记日志 —— 那边失败，这边接得住。
var ErrNoFullTextIndex = errors.New("videos 上没有可用的 FULLTEXT(title, description) 索引")

// LexicalSearcher 词法那一路。只读 videos 表，不拥有它。
//
// 放在 search 包而不是 feed 包：feed 是"读视图"层，它读 videos 是为了组装
// feed 响应；而这里是**检索算法**的一部分（怎么构造 BOOLEAN 查询、AND 不够时怎么退 OR），
// 换一个入口（比如将来的 /video/search）也应该复用同一份。
type LexicalSearcher struct {
	db *gorm.DB
}

func NewLexicalSearcher(db *gorm.DB) *LexicalSearcher {
	return &LexicalSearcher{db: db}
}

// scoredID 查询结果的临时形状：一条视频 id + 它在这一路里的分数。
// 匿名的 struct slice 也能 Scan，但给个名字更省事，也让 gorm 的列名映射有处可写。
type scoredID struct {
	ID    uint    `gorm:"column:id"`
	Score float64 `gorm:"column:score"`
}

// Search 跑词法这一路，返回 (排好序的列表, **实际跑成的模式**, 错误)。
//
// 第二个返回值不是装饰：使用者必须如实把它报出去。AND 和 OR 的差别对用户是可见的
// （AND 是"全都得有"，OR 是"有就行"，结果集宽窄差很多），
// 悄悄从 AND 掉到 OR 而用户以为还是精确匹配，就会出现"搜什么都有一堆结果"的困惑。
func (s *LexicalSearcher) Search(ctx context.Context, q Query, depth int) (RankedList, Mode, error) {
	// 查询词太短（CompileQuery 已经判过）→ 直接 LIKE，不碰全文索引
	if q.UseLike {
		list, err := s.searchLike(ctx, q.LikePatterns, depth)
		if err != nil {
			return nil, "", err
		}
		return list, ModeLike, nil
	}

	list, err := s.searchMatch(ctx, q.MatchAnd, depth)
	if err != nil {
		return nil, "", err
	}
	if len(list) >= depth {
		return list, ModeNgram, nil
	}

	// AND 没喂饱 depth → 用 OR 重试一次，把"严格匹配"放宽成"部分匹配"。
	//
	// 这一步就是"搜索引擎"和"数据库精确查询"的分界线：
	// 用户搜 "红烧肉 做法"，库里只有一条写了"红烧肉"、没有一条同时含两个词时，
	// 严格 AND 会回空列表（用户看到"没有找到"），而搜索引擎会给他那条红烧肉。
	// 代价是排序质量下降（部分匹配的结果混进来），所以**只在 AND 不够时才退**，
	// 而不是一上来就用 OR。
	orList, err := s.searchMatch(ctx, q.MatchOr, depth)
	if err != nil {
		// OR 那一次失败不该抹掉已经拿到的 AND 结果 —— 降级而不是失败。
		// 这种"第二段重试失败但第一段有效"的情况在数据库上不常见，
		// 但把它变成 500 是明显的过度反应
		return list, ModeNgram, nil
	}
	// OR 理论上一定 ⊇ AND，但真出现更少（比如中途有视频被删）就保留原结果，
	// 别让一个"重试"把结果变差
	if len(orList) < len(list) {
		return list, ModeNgram, nil
	}
	if len(orList) > 0 {
		return orList, ModeNgramOr, nil
	}

	// ---------- 第三档：LIKE 兜底 ----------
	//
	// 走到这里意味着 AND 和 OR **都是空集** —— 全文索引对这批 token 一条都没命中。
	//
	// 这时**不能直接返回空**。空集的语义是"库里没有这个视频"，而真实原因
	// 分三种，且都不是"没有"：
	//
	//	① 索引按含停用词建的（"java" 的每个 bigram 都含 a，全被丢掉）
	//	   —— 本项目踩过的那个，见 video/search_index.go 的坑 #2 / 坑 #5
	//	② ngram_token_size 被人改过，索引里的 token 长度和查询对不上
	//	③ 索引给已有数据的表建完没重建，内容不全
	//
	// 三种都是"索引不可信"，而 LIKE 全表扫**不看索引**，所以它是这一档唯一
	// 还能给出正确答案的手段。代价是一次全表扫 —— 但这种查询本来就是要
	// 返回空的，没有"更贵"可言；而 127 行的表上，代价可以忽略。
	//
	// 这正是本项目一以贯之的那条：**降级要说出来，不要悄悄返回空**。
	// 报 ModeLikeFallback 而不是 ModeLike，就是为了让"索引可疑"这件事
	// 在调试面板上看得见 —— 否则这个 bug 会永远藏在"搜不到就是没有"里。
	likeList, err := s.searchLike(ctx, q.LikePatterns, depth)
	if err != nil {
		// 兜底自己失败不该把整路弄挂：两路召回的结构里，词法这一路返回错误
		// 会让 service 退成纯向量（见 service.go 的 lexErr 分支）。既然
		// 我们已经有一个（空）结果，报它比报错更贴近事实。
		log.Printf("[search] LIKE 兜底失败，本次返回空集: %v", err)
		return orList, ModeNgramOr, nil
	}
	if len(likeList) > 0 {
		log.Printf("[search] 全文索引零命中，LIKE 兜底救回 %d 条 —— "+
			"索引可疑，查 SHOW CREATE TABLE videos 的 FULLTEXT COMMENT 与 @@ngram_token_size", len(likeList))
	}
	return likeList, ModeLikeFallback, nil
}

// searchMatch 走全文索引的查询：相关度倒序 + (create_time, id) 兜底。
//
// ---------- 占位符的顺序 ----------
//
// 这里**同一个表达式出现了两次**（SELECT 里一次算分数、WHERE 里一次用于过滤），
// 所以传参必须按 "SELECT 的 ? 先在、WHERE 的 ? 在后" 的顺序给：
//
//	[0] boolean ← SELECT 里那个
//	[1] boolean ← WHERE 里那个
//	（LIMIT 的 depth 是**字面量**不是占位符：GORM 的 clause.Limit 对 int
//	  直接写进 SQL，不 AddVar —— 所以它不占参数位）
//
// 搞错会得到什么：GORM 的 clause.Expr 在 ? 多于已注册变量时，会把剩下的 `?`
// **原样写进 SQL 文本**（不是报"参数数量不匹配"）。所以是 MySQL 语法错误，
// 不是静默的错误结果 —— 这一点算是运气。
//
// **WHERE 里必须重复整个 MATCH 表达式，不能引用 SELECT 的别名。**
// SQL 在这件事上是不对称的：ORDER BY 可以用别名（`ORDER BY score DESC` 合法），
// WHERE 不行。这是这一段最容易写错的地方。
//
// ---------- 关于 ORDER BY 的三键 ----------
//
// score 之后还要跟 create_time, id：MATCH 分数是浮点数，**同分是常态**
// （尤其在小语料上，一堆文档的 bigram 命中数一样）。只按 score 排的话，
// 同分行之间的顺序由 MySQL 自由决定 —— 每次查询可能不一样，
// 而这一路的顺序会通过"名次"直接决定 RRF 的分数，于是**同一轮搜索翻页时
// 结果会跳来跳去**。补上唯一键 id 之后顺序才是全序的。
func (s *LexicalSearcher) searchMatch(ctx context.Context, boolean string, depth int) (RankedList, error) {
	var rows []scoredID
	err := s.db.WithContext(ctx).Model(&video.Video{}).
		Select("id, MATCH(title, description) AGAINST (? IN BOOLEAN MODE) AS score", boolean).
		Where("MATCH(title, description) AGAINST (? IN BOOLEAN MODE)", boolean).
		Order("score DESC, create_time DESC, id DESC").
		Limit(depth).
		Scan(&rows).Error
	if err != nil {
		if isNoFullTextIndex(err) {
			return nil, ErrNoFullTextIndex
		}
		return nil, err
	}

	list := make(RankedList, 0, len(rows))
	for _, r := range rows {
		list = append(list, Ranked{ID: r.ID, Score: r.Score})
	}
	return list, nil
}

// searchLike 降级那一路：LIKE，时间倒序。
//
// 它的存在有三个理由，而且是**互相独立**的三个：
//
//	① 查询词短于 ngram_token_size（单字、`C++`）—— 全文索引里没有对应的 token，
//	   走 MATCH 一定返回空集且不报错。单字搜索是中文用户的自然输入，必须有路可走。
//	② 全文索引没建成功（没权限 / MyISAM / 被人删了）—— ErrNoFullTextIndex。
//	③ **全文索引在、但零命中** —— 索引内容不可信（停用词削掉了 bigram、
//	   ngram_token_size 被改过、建完没重建）。见 Search 里的第三档。
//
// 单独用 ① 一个理由就足够让它存在了 —— 这也是为什么它必须写在这里，
// 而不能指望"将来上 Elasticsearch 了就不需要了"。②③ 是它顺带接住的。
//
// ---------- 为什么是多个模式 ----------
//
// patterns 里的每个模式都要满足（AND 连起来），语义和 MatchAnd 对齐。
// 只有一个模式时退化成原来的形状，行为不变。
//
// ESCAPE 的字符从常量拼进来，不写字面量 '!' —— 两处必须一致，
// 而这种"两处常量手工同步"正是最容易在改动时漏掉的地方。
//
// Score 恒为 0：LIKE 没有相关度这个概念，顺序纯粹是时间倒序。
// 调用方**不要**把这个 0 当成"相关度是 0"往外报（那会让人以为全都不相关）。
func (s *LexicalSearcher) searchLike(ctx context.Context, patterns []string, depth int) (RankedList, error) {
	type idRow struct {
		ID uint `gorm:"column:id"`
	}
	var rows []idRow

	if len(patterns) == 0 {
		// 没有模式就**不要发一条 WHERE (1=1) 的全表扫**。理论上调用方不会
		// 传空（CompileQuery 保证至少有一个），但"理论上不会"不是不发问的理由：
		// 空切片会让下面 strings.Join 出空串，拼出 `WHERE ` 结尾的非法 SQL。
		return RankedList{}, nil
	}

	clauses := make([]string, 0, len(patterns))
	args := make([]any, 0, len(patterns)*2)
	for _, p := range patterns {
		clauses = append(clauses, fmt.Sprintf("(title LIKE ? ESCAPE '%s' OR description LIKE ? ESCAPE '%s')",
			likeEscapeChar, likeEscapeChar))
		args = append(args, p, p)
	}

	err := s.db.WithContext(ctx).Model(&video.Video{}).
		Select("id").
		Where(strings.Join(clauses, " AND "), args...).
		Order("create_time DESC, id DESC").
		Limit(depth).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	list := make(RankedList, 0, len(rows))
	for _, r := range rows {
		list = append(list, Ranked{ID: r.ID, Score: 0})
	}
	return list, nil
}

// isNoFullTextIndex 判断是不是 MySQL 1191 (ER_FT_MATCHING_KEY_NOT_FOUND)。
//
// 和 like_repo.go 的 isDupKey 一样：**比错误码，绝不比 err.Error() 的文案** ——
// 文案会随版本、语言、甚至 MySQL 的发行版变，错误码不会。
func isNoFullTextIndex(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == 1191
}
