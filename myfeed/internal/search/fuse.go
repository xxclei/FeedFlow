package search

import "sort"

// FuseRRF 把多路召回融成一个 id 列表。Reciprocal Rank Fusion，Cormack 等 2009。
//
//	score(d) = Σ_i  1 / (k + rank_i(d))        rank 从 1 开始
//
// ---------- 为什么是 RRF 而不是加权求和 ----------
//
// 两路的分数**量纲不可比**：词法那一路是 BM25 相关度、无上界（同一批文档里
// 一个词的命中能把分数顶到 10，换一组查询可能全在 0.1 量级）；语义那一路是
// 余弦距离、被死死限制在 [0,2]。加权求和要先归一化到同一尺度、还要按语料
// 反复调参，而**归一化本身就是有损的**（它依赖本次查询的分数分布，
// 于是同一个文档在不同查询里的相对权重会漂移）。
//
// RRF 只看**名次**，天然免疫这一切 —— 这就是它十几年后仍然是各家默认融合算法的原因。
//
// ---------- 三个必须在代码里点明的细节 ----------
//
//  1. **RRF 丢掉了分数的量级**，所以一路"最不坏的无关结果"也能拿到完整的
//     1/(k+rank) 分。这就是 VectorStore.KNN 里那个 maxDistance 门槛存在的理由：
//     向量那一路总能凑满 k 个"最近"的结果，哪怕最近的也很远。
//  2. **空的一路要整个跳过**（下面的 continue）。注意这一点是**为了可读性**，
//     不是正确性：空列表本来就不贡献分数。写出来是为了让"两路都空"和"一路空"
//     在读代码时一眼可分 —— 而这两种情况的日志含义完全不同。
//  3. **融合结果必须全序**。同分时用 id 降序兜底：id 是唯一键，
//     拿它做最后一级比较保证任意两个 id 的先后永远确定。
//     少了这一级，同一个查询两次跑的排序可能不一样，而顺序会被冻结进游标
//     （见 cursor.go）—— 于是第一页和第二页之间可能出现重复或遗漏。
//     取降序而不是升序，是为了和项目里其它流一致（新的内容优先）。
func FuseRRF(k int, lists ...RankedList) []uint {
	scores := make(map[uint]float64, 64)
	// firstSeen 只负责"保持 id 的首次出现顺序"，实际排序在下面按分数重做。
	// 用一个切片而不是遍历 map：**map 的遍历顺序是随机的**，
	// 直接拿 map 排序会让同分项的顺序在两次调用之间变化
	firstSeen := make([]uint, 0, 64)

	for _, list := range lists {
		if len(list) == 0 {
			continue
		}
		for i, r := range list {
			if _, seen := scores[r.ID]; !seen {
				firstSeen = append(firstSeen, r.ID)
			}
			scores[r.ID] += 1.0 / float64(k+i+1)
		}
	}

	sort.Slice(firstSeen, func(i, j int) bool {
		a, b := firstSeen[i], firstSeen[j]
		if scores[a] != scores[b] {
			return scores[a] > scores[b] // 分数高的在前
		}
		return a > b // 同分：id 大的在前（唯一键兜底，保证全序）
	})
	return firstSeen
}
