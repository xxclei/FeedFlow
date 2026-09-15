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

// ftIndexComment 生成"这个索引是按什么配方建的"的指纹，写进索引自己的 COMMENT。
//
// ---------- 为什么要有指纹 ----------
//
// 这个索引的内容**不只是列决定**，还取决于两个不写在 DDL 里的东西：
//
//	innodb_ft_enable_stopword  建索引那一刻的值（决定含停用词的 n-gram 是否被丢）
//	ngram_token_size           切多长的 token（决定索引里 token 的形状）
//
// 而这两个值都**不能从 information_schema 里读出来**。后果是：索引建错了，
// 服务端只能"看到它存在"然后放心地用，搜不到东西也不报错 ——
// 这正是本项目踩过的那个坑（见下面坑 #5），当时的服务端只能打一句
// "解析器未能自动校验，请人工确认一次"，而**没有人会去人工确认**。
//
// 所以改成让索引自己记着配方：把配方写在 COMMENT 里（COMMENT 是
// information_schema.STATISTICS.INDEX_COMMENT 能读到的），
// 启动时拿它和"现在应该用的配方"比。**不一致就重建。**
// 判据跟着变量走了，不需要任何人的记性 —— 靠结构，不靠纪律。
//
// 配方里为什么必须有 ngram_token_size：它是**全局变量**，改了不会让已有索引
// 报任何错，只会让它搜不出东西（索引里是 2-gram、查询按 3-gram 切，两边对不上）。
// 又一条静默失效，同类问题一并堵掉。
//
// 格式 `myfeed-ft:ngram-n2-sw0` 里的 `sw0` 表示"建的时候停用词是关的"。
// 本项目**永远是关的**（理由见坑 #2），所以这个位置是常量而不是变量：
// 哪天有人要改成 ON，改动点就在这一行，而所有旧索引会因此指纹不符、自动重建。
func ftIndexComment(db *gorm.DB) string {
	// ngram_token_size 读不到时按 2 兜底：这是本项目的实际取值，
	// 而真读不到的话（MySQL 没有 ngram 插件）下面的 ALTER 也会失败并记日志，
	// 不会因为这一个数而静默降级。这里不 return err 是为了不让"读一个变量失败"
	// 变成"整个索引的检查都跳过"。
	var n int
	if err := db.Raw("SELECT @@ngram_token_size").Row().Scan(&n); err != nil || n <= 0 {
		log.Printf("[search] 读 @@ngram_token_size 失败，指纹按 2 算: %v", err)
		n = 2
	}
	return fmt.Sprintf("myfeed-ft:ngram-n%d-sw0", n)
}

// EnsureFulltextIndex 幂等地把 videos 的 ngram 全文索引弄成**可用的**。由 db.AutoMigrate 调用。
//
// 注意措辞是"弄成可用的"，不是"建出来"：索引存在但配方不对时，这里会把它重建。
// 早先的版本只做"不存在才建"，于是索引一旦建错就**永久错下去**（见坑 #5）。
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
// ---------- 五个真实的坑（按会咬人的顺序） ----------
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
//     比上面那段更狠的是**单个字母**：默认停用词表里还有 `a` 和 `i`（长度 1，
//     按字面看像是无关紧要的条目），而 ngram_token_size=2 时"含停用词"的判定是
//     **子串**判定 —— 含字母 a 或 i 的 bigram 全都被丢掉。于是：
//
//     "java"   → ja / av / va   **三个 bigram 全含 a，全丢** → 一条都搜不到
//     "banana" → 同理，全丢
//     "redis"  → re ✓ / ed ✓ / di（含 i）✗ / is（本身是停用词）✗ → 还剩两个，能搜到
//
//     所以"java 搜不到而 redis 搜得到"不是巧合、也不是数据问题，是这条规则。
//
//  3. **不要在 BOOLEAN 查询里用引号短语**（MySQL Bug #118238，8.0.30，ngram + utf8mb4）：
//     短语里含 CJK 标点且不在首位时，`IN BOOLEAN MODE` **返回空集**，
//     而同样内容不带引号能正确命中。所以 lexical.go 构造查询时用**裸 token**。
//
//  4. **`*` 通配符先不加**。ngram 本身已经把词切成 bigram 了，
//     再叠 `*` 的行为是版本相关的，且有"搜索词是句末字符时 `*` 失效"的报告。
//     先用裸 `+token`，实测漏检再谈。验收清单里留了对比这两者的 SQL。
//
//  5. **`OPTIMIZE TABLE` 会把停用词设置"按当前会话重新应用一遍"** ——
//     这是坑 #2 的续集，也是本项目真的踩进去的那个：早先的写法是
//     "建索引时 SET SESSION ... = OFF，然后**出了这个连接**再 OPTIMIZE"，
//     而池里拿到的下一条连接是默认的 ON。OPTIMIZE 在 InnoDB 上被实现成
//     ALTER TABLE ... FORCE，**整表重建包含全文索引** —— 于是刚用"关停用词"
//     建好的索引，紧接着被"开停用词"重建了一遍，SET SESSION 那一行等于没写。
//     MySQL 自己会说这句：`Table does not support optimize, doing recreate + analyze instead`。
//
//     教训不是"OPTIMIZE 有毒"，而是：**凡是要写索引内容的那一步，都必须和
//     关停用词的那一步在同一条连接上**。下面的写法把 SET / DROP+ADD / OPTIMIZE
//     全部塞进同一个 db.Connection 闭包，就是为了让这条约束由结构保证。
//
// ---------- 失败时只记日志，不阻断启动 ----------
//
// 搜索是**可降级**的功能（和 Redis/MQ 在本项目里的地位一样）：
// 没有 ALTER 权限（1142）、或者表是 MyISAM（ngram 不支持），
// 都不该让整个服务起不来。查询时还有第二道降级：投不出全文索引的 1191
// 会被 lexical_repo.go 认出来，搜索退化成纯向量那一路；
// 索引在但搜不出东西（本坑的形态）则由 lexical_repo.go 的**第三档 LIKE 兜底**接住。
// 三道防线里，这一道负责"别再建错"，不是"建错了别炸"。
func EnsureFulltextIndex(db *gorm.DB) error {
	type ftRow struct {
		IndexName    string `gorm:"column:index_name"`
		ColumnName   string `gorm:"column:column_name"`
		SeqInIndex   int    `gorm:"column:seq_in_index"`
		IndexComment string `gorm:"column:index_comment"`
	}

	var rows []ftRow
	// information_schema.STATISTICS 是**每列一行**，所以要在 Go 里按 INDEX_NAME 归组，
	// 比的是列集合而不是索引名（理由见下）
	err := db.Raw(`
		SELECT INDEX_NAME    AS index_name,
		       COLUMN_NAME   AS column_name,
		       SEQ_IN_INDEX  AS seq_in_index,
		       INDEX_COMMENT AS index_comment
		  FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME   = 'videos'
		   AND INDEX_TYPE   = 'FULLTEXT'
		 ORDER BY INDEX_NAME, SEQ_IN_INDEX`).Scan(&rows).Error
	if err != nil {
		log.Printf("[search] 查 FULLTEXT 索引失败，跳过创建: %v", err)
		return nil
	}

	type ftIndex struct {
		cols    []string
		comment string
	}
	byIndex := map[string]*ftIndex{}
	for _, r := range rows {
		idx := byIndex[r.IndexName]
		if idx == nil {
			idx = &ftIndex{}
			byIndex[r.IndexName] = idx
		}
		idx.cols = append(idx.cols, r.ColumnName)
		// 注释在索引的每一行上都重复一份，取到非空的即可
		if r.IndexComment != "" {
			idx.comment = r.IndexComment
		}
	}

	want := ftIndexComment(db)

	// staleName 是"列对得上、但配方不对"的那个索引 —— 下面要在同一个 ALTER 里
	// 把它换成新的。只认**列集合**对得上的：别人（或上一个版本）用另一个名字
	// 建了同一对列的全文索引，按名字判会让我们再建一个完全重复的 ——
	// MySQL 不报错，只是白维护一棵 B+ 树。
	var staleName string
	for name, idx := range byIndex {
		if !slices.Equal(idx.cols, ftIndexColumns) {
			continue
		}
		if idx.comment == want {
			log.Printf("[search] 全文索引 %s 已就绪（配方 %s），跳过创建", name, want)
			return nil
		}
		staleName = name
		log.Printf("[search] 全文索引 %s 的配方是 %q，应为 %q —— 将重建。"+
			"（配方不符 = 索引内容是按别的停用词/切词长度建的，搜不出东西且不报错）",
			name, idx.comment, want)
	}

	// ---------- 写索引内容的三步，必须钉在同一条连接上 ----------
	//
	// 坑 #2 和坑 #5 都源于"这几步跑在了不同的连接上"。db.Exec 每次可能落到
	// 连接池里不同的连接上，两次 Exec 之间连接被换掉**不报错**，
	// 只是 SET 白设了。db.Connection 把整个闭包钉死在一条 *sql.Conn 上。
	err = db.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec("SET SESSION innodb_ft_enable_stopword = OFF").Error; err != nil {
			return err
		}
		// 恢复会话变量：对下面这几步已经没影响了（它们都已经读过了），
		// 纯粹是为了不给连接池留一个"被别人改过"的连接 ——
		// 那个连接之后会被别的请求复用，而它身上的会话状态没有第二个人知道。
		// 用 defer 而不是顺序执行：中间任何一步 return 都会跳过恢复。
		defer func() {
			if err := tx.Exec("SET SESSION innodb_ft_enable_stopword = ON").Error; err != nil {
				log.Printf("[search] 恢复 innodb_ft_enable_stopword 失败（该连接回池后仍为 OFF）: %v", err)
			}
		}()

		// 配方不对的旧索引和新的合成**一条 ALTER**：DROP + ADD 一次表重建就完事，
		// 分两条写会让 MySQL 重建两次表（127 行无所谓，但这是白给的代价）。
		ddl := fmt.Sprintf("ALTER TABLE videos ADD FULLTEXT INDEX %s (title, description)"+
			" WITH PARSER ngram COMMENT '%s'", videoFTIndexName, want)
		if staleName != "" {
			ddl = fmt.Sprintf("ALTER TABLE videos DROP INDEX `%s`,"+
				" ADD FULLTEXT INDEX %s (title, description)"+
				" WITH PARSER ngram COMMENT '%s'", staleName, videoFTIndexName, want)
		}
		if err := tx.Exec(ddl).Error; err != nil {
			return err
		}

		// 坑 #1：给已有数据的表加 ngram 索引，索引内容不一致，必须重建一次。
		// InnoDB 的 OPTIMIZE TABLE 会重建成整张表（不是只重建全文索引）——
		// 想只重建全文索引要先 `SET GLOBAL innodb_optimize_fulltext_only=1`，
		// 那是个**全局**开关，会在整个实例上生效，为 127 行数据不值得动它。
		//
		// ⚠ 这一句就是坑 #5：它会**按当前会话的停用词设置重新切词**，
		// 所以它必须留在上面那个 SET ... = OFF 的作用域里。挪出去（哪怕只是
		// 挪到下面 db.Exec 那一层）就会把整个索引的停用词过滤重新打开，
		// 而 fingerprints 会照旧写进 COMMENT —— 一个"配方写着 sw0、
		// 内容其实是 sw1"的索引，比明摆着建错更难查。
		//
		// 失败不 return：索引已经建好了，只是可能不完整。降级为警告，
		// 用户可以手动再跑一次。
		if err := tx.Exec("OPTIMIZE TABLE videos").Error; err != nil {
			log.Printf("[search] OPTIMIZE TABLE videos 失败，索引可能不完整。"+
				"手动跑一次后再验搜索: %v", err)
		}
		return nil
	})
	if err != nil {
		log.Printf("[search] 建全文索引失败，搜索将退化为纯向量那一路: %v", err)
		return nil
	}

	if staleName != "" {
		log.Printf("[search] 已重建全文索引 %s（旧索引 %s 的配方不符）: %s",
			videoFTIndexName, staleName, want)
	} else {
		log.Printf("[search] 已创建全文索引 %s (title, description) WITH PARSER ngram（%s）"+
			"（建索引时停用词已关闭）", videoFTIndexName, want)
	}
	return nil
}
