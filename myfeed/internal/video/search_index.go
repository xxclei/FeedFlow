package video

import (
	"fmt"
	"log"
	"slices"

	"gorm.io/gorm"
)

// videoFTIndexName 全文索引的名字。
//
// 起这个名字（而不是 GORM 自动生成的 idx_videos_xxx）是刻意的：
// 它**不是**由结构体 tag 声明的，用一个人工可辨识的名字，
// 让 `show index from videos` 的输出里一眼能认出"这个索引的来历在别处"。
const videoFTIndexName = "ft_videos_title_desc"

// ftIndexColumns 全文索引要覆盖的列，顺序有意义。
//
// MATCH(title, description) 要求索引里**恰好是这两列、且顺序一致**，
// 顺序反了 MySQL 会报 1191（Can't find FULLTEXT index matching the column list）。
var ftIndexColumns = []string{"title", "description"}

// EnsureFulltextIndex 幂等地给 videos 建 ngram 全文索引。由 db.AutoMigrate 调用。
//
// ---------- 这是全项目第一处手写的 DDL ----------
//
// 到目前为止，"GORM 结构体 tag 是 schema 的唯一声明方式"这条性质一直成立
// （仓库里没有任何 .sql 文件，所有索引都在 entity 里）。**从这里开始它不成立了**：
// FULLTEXT ... WITH PARSER ngram 没有对应的 GORM tag，只能手写。
// 后果要认下来：将来改 title/description 的列名或类型时，
// 这里不会被编译器、也不会被 AutoMigrate 提醒 —— 只能靠人记住。
//
// 唯一值得庆幸的是**方向**：GORM 的 AutoMigrate 只会为"模型里声明过的"索引
// 调 CreateIndex，**从不删**它不认识的索引（gorm/migrator/migrator.go 里只看
// ParseIndexes 的结果）。所以这个手工索引不会被下一次 AutoMigrate 干掉。
//
// ---------- 为什么不用普通 FULLTEXT ----------
//
// MySQL 默认的全文解析器按**空格和标点**切词。中文句子中间没有空格，
// 所以整句"今天做了一个红烧肉"会被当成**一个 token**：
// 索引建出来了、MATCH 也跑了、**不报任何错**，只是搜"红烧肉"永远搜不到。
// 这是最坏的一类失败 —— 看着一切正常。
// ngram 解析器把文本切成 n 个字的滑窗（本机 ngram_token_size=2，即 bigram），
// 中文才搜得动。代价是索引膨胀，以及下面那一堆坑。
//
// ---------- 四个真实的坑（按会咬人的顺序） ----------
//
//  1. **给已有数据的表加 ngram 索引，索引内容会不一致** —— MySQL 文档明确说了，
//     必须接着跑一次 OPTIMIZE TABLE 重建。本项目 videos 表已经有 127 行，
//     所以这条路一定会走。下面老老实实跑了。
//
//  2. **停用词必须在建索引之前关**。ngram 会把**含停用词的 n-gram 整条丢掉**，
//     而这些 token 是**索引期**就没了的 —— 事后再改这个变量对已建的索引无效，
//     只能重建。InnoDB 默认停用词表里有 14 个**两字**词（an/as/at/be/by/de/en/in/is/it/of/on/or/to），
//     而 ngram_token_size=2 意味着**每个 token 都是两字**，所以：
//     搜 "to"/"in"/"on" 永远没结果；
//     英文单词里含这些 bigram 的会被削掉一块（"modern" 含 "de"，"sandbox" 含 "an"）。
//     中文不受影响 —— 所以这个 bug 只在英文数据上现形，更难发现。
//     OPTIMIZE 不会重建"被丢掉的 token"，所以顺序不能反。
//
//  3. **不要在 BOOLEAN 查询里用引号短语**（MySQL Bug #118238，8.0.30，ngram + utf8mb4）：
//     短语里含 CJK 标点且不在首位时，`IN BOOLEAN MODE` **返回空集**，
//     而同样内容不带引号能正确命中。所以 lexical.go 构造查询时用**裸 token**。
//
//  4. **`*` 通配符先不加**。ngram 本身已经把词切成 bigram 了，
//     再叠 `*` 的行为是版本相关的，且有"搜索词是句末字符时 `*` 失效"的报告。
//     先用裸 `+token`，实测漏检再谈。验收清单里留了对比这两者的 SQL。
//
// ---------- 失败时只记日志，不阻断启动 ----------
//
// 搜索是**可降级**的功能（和 Redis/MQ 在本项目里的地位一样）：
// 没有 ALTER 权限（1142）、或者表是 MyISAM（ngram 不支持），
// 都不该让整个服务起不来。查询时还有第二道降级：投不出全文索引的 1191
// 会被 lexical_repo.go 认出来，搜索退化成纯向量那一路。
func EnsureFulltextIndex(db *gorm.DB) error {
	type ftRow struct {
		IndexName  string `gorm:"column:index_name"`
		ColumnName string `gorm:"column:column_name"`
		SeqInIndex int    `gorm:"column:seq_in_index"`
	}

	var rows []ftRow
	// information_schema.STATISTICS 是**每列一行**，所以要在 Go 里按 INDEX_NAME 归组，
	// 比的是列集合而不是索引名（理由见下）
	err := db.Raw(`
		SELECT INDEX_NAME  AS index_name,
		       COLUMN_NAME AS column_name,
		       SEQ_IN_INDEX AS seq_in_index
		  FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME   = 'videos'
		   AND INDEX_TYPE   = 'FULLTEXT'
		 ORDER BY INDEX_NAME, SEQ_IN_INDEX`).Scan(&rows).Error
	if err != nil {
		log.Printf("[search] 查 FULLTEXT 索引失败，跳过创建: %v", err)
		return nil
	}

	byIndex := map[string][]string{}
	for _, r := range rows {
		byIndex[r.IndexName] = append(byIndex[r.IndexName], r.ColumnName)
	}
	for name, cols := range byIndex {
		if slices.Equal(cols, ftIndexColumns) {
			// 判"存在"按**列集合**而不是按名字：如果别人（或上一个版本）用另一个名字
			// 建了同一对列的全文索引，按名字判会让我们再建一个完全重复的 ——
			// MySQL 不报错，只是白维护一棵 B+ 树。
			//
			// 但解析器是不是 ngram，information_schema 里查不到（STATISTICS 不暴露 parser），
			// 只能 `SHOW CREATE TABLE videos` 看有没有 WITH PARSER `ngram`。
			// 所以这里**只提醒、不自动重建**：DROP + ADD 要重建整个索引，代价不对称，
			// 而这个判断只在"有人手工动过索引"时才为真。
			log.Printf("[search] 全文索引已存在，跳过创建: %s(%v)。"+
				"解析器未能自动校验，请人工确认一次: SHOW CREATE TABLE videos", name, cols)
			return nil
		}
	}

	ddl := fmt.Sprintf("ALTER TABLE videos ADD FULLTEXT INDEX %s (title, description) WITH PARSER ngram",
		videoFTIndexName)

	// SET 和 ALTER 必须在**同一条连接**上执行。
	// innodb_ft_enable_stopword 是会话变量、且只在"建索引那一刻"被读取，
	// 而 db.Exec 每次可能落到连接池里不同的连接上 —— 两次 Exec 之间
	// 连接被换掉的话，停用词设置就白设了，而且**不报错**。
	// db.Connection 把整个闭包钉死在一条 *sql.Conn 上。
	err = db.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec("SET SESSION innodb_ft_enable_stopword = OFF").Error; err != nil {
			return err
		}
		alterErr := tx.Exec(ddl).Error
		// 恢复会话变量：对本次 ALTER 已经没影响了（它读过了），
		// 纯粹是为了不给连接池留一个"被别人改过"的连接 ——
		// 那个连接之后会被别的请求复用，而它身上的会话状态没有第二个人知道
		if err := tx.Exec("SET SESSION innodb_ft_enable_stopword = ON").Error; err != nil {
			log.Printf("[search] 恢复 innodb_ft_enable_stopword 失败（该连接回池后仍为 OFF）: %v", err)
		}
		return alterErr
	})
	if err != nil {
		log.Printf("[search] 建全文索引失败，搜索将退化为纯向量那一路: %v", err)
		return nil
	}

	// 坑 #1：给已有数据的表加 ngram 索引，索引内容不一致，必须重建一次。
	// InnoDB 的 OPTIMIZE TABLE 会重建成整张表（不是只重建全文索引）——
	// 想只重建全文索引要先 `SET GLOBAL innodb_optimize_fulltext_only=1`，
	// 那是个**全局**开关，会在整个实例上生效，为 127 行数据不值得动它。
	// 这一步只在"刚刚真的建了索引"的分支里跑，所以每次启动的代价是 0。
	if err := db.Exec("OPTIMIZE TABLE videos").Error; err != nil {
		log.Printf("[search] OPTIMIZE TABLE videos 失败，索引可能不完整。"+
			"手动跑一次后再验搜索: %v", err)
	}

	log.Printf("[search] 已创建全文索引 %s (title, description) WITH PARSER ngram"+
		"（建索引时停用词已关闭）", videoFTIndexName)
	return nil
}
