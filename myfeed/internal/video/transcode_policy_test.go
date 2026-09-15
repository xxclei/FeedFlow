package video

// 上传质量门禁的判定策略测试。
//
// ---------- 为什么是**内部测试包**（package video）而不是 video_test ----------
//
// 和 like_concurrency_test.go 正好相反，那个必须是外部包（避免
// video → db → video 的导入环）。这个**必须是内部包**，理由只有一个：
//
//	它要测的 decideTranscode / shortSide / targetForShortSide / usableTiers
//	**全是未导出的**。
//
// 这也是当初把它们写成纯函数的原因 —— 没有 IO、没有时间、没有随机，
// 所以可以在这里把每个分支和每个边界都摆出来，不需要数据库、不需要
// config.yaml、不需要 ffmpeg。`go test ./internal/video/ -run TestDecide -v`
// 从任意 CWD 都能跑（不像外部包那些测试要往上退两级找 config）。
//
// ---------- 这个文件在防什么 ----------
//
// transcode_policy.go 是整个扩展的**分流器**：它的输出直接决定
// "这条视频走 HLS 还是走原文件"。判错的两种后果不对称：
//
//	该转的没转 → 一条 12 Mbps 原片按原样服务，撑 1 个人（回到改造前）
//	不该转的转了 → 白烧几十分钟 CPU，画质还更差（转码是有损的）
//
// 两种都不报错，都是"跑起来了但结果不对"。所以这里按分支穷举。

import "testing"

// ---------- 辅助 ----------

// mp4 造一个探测结果。参数顺序按"读起来最顺"排：先尺寸后码率。
func mp4(w, h, kbps int) *MP4Info {
	return &MP4Info{Width: w, Height: h, BitrateKbps: kbps, DurationMS: 30000, SizeBytes: 1 << 20}
}

// ---------- decideTranscode：五个分支逐个覆盖 ----------

func TestDecideTranscode(t *testing.T) {
	tests := []struct {
		name string
		// 入参
		info    *MP4Info
		probeOK bool
		// 期望
		wantTranscode bool
		// wantReason 只对**不转**的用例断言（那才是要给人解释的情况）。
		// 转的用例理由里带数字，钉死它只会让调阈值时多改几处。
		wantReason string
	}{
		// ---------- 分支 1：探测失败 ----------
		{
			name:    "探测失败/文件损坏 → 直传",
			info:    nil,
			probeOK: false,
			// 保守取向：对坏文件跑分钟级 ffmpeg，多半也是失败，白烧 CPU。
			wantTranscode: false,
			wantReason:    "probe failed",
		},
		{
			// 防线冗余：调用方理论上不会传 probeOK=true + info=nil，
			// 但真传了也不能 panic（那会在 Publish 里炸掉整个发布流程）。
			name:          "probeOK 但 info 为 nil → 直传且不 panic",
			info:          nil,
			probeOK:       true,
			wantTranscode: false,
			wantReason:    "probe failed",
		},

		// ---------- 分支 2：分辨率超出最大可服务档（码率无关）----------
		{
			name: "4K 且码率很低 → 仍然转（分辨率那条覆盖码率那条）",
			// 这一条专门钉死**判定顺序**：3840x2160 的 short side 是 2160，
			// 超过 maxServeShortSide=1080。即使它码率只有 800 kbps
			// （低于 1080p 档的 4500），也必须转 —— 我们根本没有 4K 档位，
			// 不转就等于按 4K 服务，远超这条 12 Mbps 上行能承受的量。
			info:          mp4(3840, 2160, 800),
			probeOK:       true,
			wantTranscode: true,
		},
		{
			// 边界：短边正好 1080 → **不**因分辨率转。
			name:          "短边正好 1080 → 分辨率这条不触发",
			info:          mp4(1920, 1080, 3000),
			probeOK:       true,
			wantTranscode: false, // 3000 <= 4500×1.25，码率那条也不触发
			wantReason:    "",
		},

		// ---------- 分支 3：比最低档还小 → 直传 ----------
		{
			name:          "360p 小文件 → 直传（把 360p 拉到 480p 只会更大更糊）",
			info:          mp4(640, 360, 360),
			probeOK:       true,
			wantTranscode: false,
			wantReason:    "shorter than lowest tier (short side 360)",
		},
		{
			// 边界：短边正好 480 = 最低档的 Height → 够得着，不因这条直传。
			name: "短边正好 480 → 够得着最低档",
			// 480p 档目标 1000 kbps，×1.25 = 1250。给 1200 就落在范围内。
			info:          mp4(854, 480, 1200),
			probeOK:       true,
			wantTranscode: false,
		},
		{
			name:          "竖屏 360×640 → 按短边 360 归档，同样直传",
			info:          mp4(360, 640, 300),
			probeOK:       true,
			wantTranscode: false,
			wantReason:    "shorter than lowest tier (short side 360)",
		},

		// ---------- 分支 4：码率不可知 → 直传 ----------
		{
			name: "mvhd 缺失（时长算不出来）→ 码率不可知 → 直传",
			// BitrateKbps 由 size/duration 得出，duration 为 0 时这里就是 0。
			// 选择直传而不是转，和分支 1 同一个保守取向：
			// 分辨率已经知道了，而"码率不明"更可能来自畸形文件。
			info:          mp4(1920, 1080, 0),
			probeOK:       true,
			wantTranscode: false,
			wantReason:    "bitrate unknown (no mvhd)",
		},
		{
			name:          "码率为负（畸形 duration）→ 同样直传",
			info:          mp4(1920, 1080, -5),
			probeOK:       true,
			wantTranscode: false,
			wantReason:    "bitrate unknown (no mvhd)",
		},

		// ---------- 分支 5：码率超过目标的可接受范围 → 转 ----------
		{
			name: "1080p 手机原片 12 Mbps → 转（这就是这个模块存在的理由）",
			// bench-notes-video-delivery.md 实测：这样一条原片把
			// "能撑 33 人"直接打到"不到 1 人"。
			info:          mp4(1920, 1080, 12000),
			probeOK:       true,
			wantTranscode: true,
		},
		{
			name: "1080p 但码率只高出一点点 → 直传（转码有损，不值）",
			// 5000 kbps vs 1080p 档 4500×1.25=5625 → 没超。
			// 重编成 4500 只会让画质更差、体积小 10%，纯亏。
			info:          mp4(1920, 1080, 5000),
			probeOK:       true,
			wantTranscode: false,
		},
		{
			// 边界：limit 是精确的 5625.0（4500×1.25），所以整数比较可以钉死。
			name:          "1080p 源码率正好落在 4500×1.25 上 → 直传（含等于）",
			info:          mp4(1920, 1080, 5625),
			probeOK:       true,
			wantTranscode: false,
		},
		{
			name:          "1080p 源码率超出 1 kbps → 转",
			info:          mp4(1920, 1080, 5626),
			probeOK:       true,
			wantTranscode: true,
		},
		{
			name: "720p 源 4 Mbps → 转（对 720p 档 2500 来说超了）",
			// 1280x720 short side 720 → 目标是 720p 档（不是 1080p），
			// 因为 1080p 档比源还大，转了就是放大。
			info:          mp4(1280, 720, 4000),
			probeOK:       true,
			wantTranscode: true,
		},
		{
			name:          "720p 源 2500 → 直传（正好等于档位目标）",
			info:          mp4(1280, 720, 2500),
			probeOK:       true,
			wantTranscode: false,
		},
		{
			name: "竖屏 1080×1920 高码率 → 转（短边 1080 归档成 1080p）",
			// 竖屏不能按 height 归档，否则 1920 会被当成"比 1080p 还大"。
			info:          mp4(1080, 1920, 12000),
			probeOK:       true,
			wantTranscode: true,
		},

		// ---------- 存量素材回归 ----------
		{
			name: "存量素材 0.36 Mbps → 直传（111 条的期望归宿）",
			// 已确认不碰存量。这条用例是那个决策的守卫：哪天有人动了
			// margin 或档位表，如果它开始"转"，111 条会被一起拉去重转。
			info:          mp4(640, 360, 360),
			probeOK:       true,
			wantTranscode: false,
			wantReason:    "shorter than lowest tier (short side 360)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideTranscode(tt.info, tt.probeOK)
			if got.Transcode != tt.wantTranscode {
				t.Fatalf("Transcode = %v, want %v（Reason=%q）",
					got.Transcode, tt.wantTranscode, got.Reason)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			// 理由字符串是给人和日志看的，但**空的理由不可接受** ——
			// 排查时第一个问题永远是"这条为什么没转"，空洞等于没写。
			if got.Reason == "" {
				t.Error("Reason 为空：判定结果必须自带理由")
			}
		})
	}
}

// ---------- targetForShortSide：档位选择的边界 ----------
//
// 这条规则单独测，是因为它错起来很安静：选错一档不会报错，
// 只会让一条 720p 的源去生成 1080p 的分片（放大，更大更糊），
// 或者反过来让 1080p 源只生成 720p（白白丢画质）。

func TestTargetForShortSide(t *testing.T) {
	tests := []struct {
		ss     int
		wantHi int // 期望档位的短边；-1 表示期望 nil
		why    string
	}{
		{ss: 2160, wantHi: 1080, why: "4K 源：最高档就是我们要服务到的 1080p"},
		{ss: 1080, wantHi: 1080, why: "正好一档"},
		{ss: 1079, wantHi: 720, why: "差 1 像素就掉一档，绝不能选 1080p（会放大）"},
		{ss: 720, wantHi: 720, why: "正好一档"},
		{ss: 719, wantHi: 480, why: "掉到最低档"},
		{ss: 480, wantHi: 480, why: "正好最低档"},
		{ss: 479, wantHi: -1, why: "比最低档还小 → 没有合适的档位"},
		{ss: 0, wantHi: -1, why: "畸形尺寸同样返回 nil，不 panic"},
	}
	for _, tt := range tests {
		got := targetForShortSide(tt.ss)
		if tt.wantHi == -1 {
			if got != nil {
				t.Errorf("targetForShortSide(%d) = %v, want nil（%s）", tt.ss, got.Name, tt.why)
			}
			continue
		}
		if got == nil {
			t.Errorf("targetForShortSide(%d) = nil, want 短边 %d（%s）", tt.ss, tt.wantHi, tt.why)
			continue
		}
		if got.Height != tt.wantHi {
			t.Errorf("targetForShortSide(%d) = %s(短边 %d), want 短边 %d（%s）",
				tt.ss, got.Name, got.Height, tt.wantHi, tt.why)
		}
	}
}

// TestTargetForShortSideNeverUpscales 把上面那条规则抽成**性质**来断言：
// 对任意短边，选中的档位都不能比源大。表驱动能覆盖具体的点，
// 这条覆盖"任意输入"—— 将来有人往 DefaultTiers 里加一档 1440p，
// 上面那张表会漏过去，这条不会。
func TestTargetForShortSideNeverUpscales(t *testing.T) {
	for ss := 0; ss <= 2160; ss++ {
		got := targetForShortSide(ss)
		if got == nil {
			continue
		}
		if got.Height > ss {
			t.Fatalf("短边 %d 选中了 %s（短边 %d）—— 会放大", ss, got.Name, got.Height)
		}
	}
}

// ---------- usableTiers：转码 worker 的档位列表 ----------

func TestUsableTiers(t *testing.T) {
	tests := []struct {
		name string
		w, h int
		want []string // 期望的档位名，从高到低
	}{
		{name: "1080p 源 → 三档全出", w: 1920, h: 1080, want: []string{"1080p", "720p", "480p"}},
		{name: "竖屏 1080×1920 → 同样三档（短边 1080）", w: 1080, h: 1920, want: []string{"1080p", "720p", "480p"}},
		{name: "720p 源 → 不出 1080p（那是放大）", w: 1280, h: 720, want: []string{"720p", "480p"}},
		{name: "480p 源 → 只出一档", w: 854, h: 480, want: []string{"480p"}},
		{name: "360p 源 → 一档都出不了", w: 640, h: 360, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UsableTiers(tt.w, tt.h)
			if len(got) != len(tt.want) {
				t.Fatalf("UsableTiers(%d,%d) 出了 %d 档 %v, want %d 档 %v",
					tt.w, tt.h, len(got), namesOf(got), len(tt.want), tt.want)
			}
			for i, tier := range got {
				if tier.Name != tt.want[i] {
					t.Errorf("第 %d 档 = %s, want %s（顺序必须从高到低）", i, tier.Name, tt.want[i])
				}
			}
		})
	}

	// 返回空切片而不是 nil 是个刻意的选择：调用方 `for range` 两者行为一样，
	// 但 `if len(x) == 0` 也一样。钉住它只是为了别哪天改成 nil 之后
	// 有人写 `if tiers == nil` 而漏掉空切片。
	if got := UsableTiers(640, 360); got == nil {
		t.Error("usableTiers 对够不着的尺寸应返回空切片而不是 nil")
	}
}

func namesOf(tiers []Tier) []string {
	out := make([]string, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, t.Name)
	}
	return out
}

// ---------- 档位表本身的守卫 ----------

// TestDefaultTiersShape 挡住"改表改歪"的几种安静错误：
// 顺序反了（usableTiers 依赖从高到低）、两档同短边（选择规则变得不确定）、
// 音轨为 0（过载核算会每档少算 128 kbps）。
func TestDefaultTiersShape(t *testing.T) {
	if len(DefaultTiers) == 0 {
		t.Fatal("档位表为空")
	}
	for i, tier := range DefaultTiers {
		if tier.Name == "" || tier.Height <= 0 || tier.VideoKbps <= 0 || tier.AudioKbps <= 0 {
			t.Errorf("第 %d 档字段不完整: %+v", i, tier)
		}
		if i > 0 {
			prev := DefaultTiers[i-1]
			if tier.Height >= prev.Height {
				t.Errorf("档位必须从高到低：%s(%d) 排在 %s(%d) 之后",
					tier.Name, tier.Height, prev.Name, prev.Height)
			}
			if tier.VideoKbps >= prev.VideoKbps {
				t.Errorf("码率也必须递减：%s(%d) 排在 %s(%d) 之后",
					tier.Name, tier.VideoKbps, prev.Name, prev.VideoKbps)
			}
		}
	}

	// TotalKbps 必须包含音轨。漏掉的表现是过载阈值系统性偏低 ——
	// 每档少算 128 kbps，看不见，只在多人时累积成"明明没超预算却开始降级"。
	for _, tier := range DefaultTiers {
		if tier.TotalKbps() != tier.VideoKbps+tier.AudioKbps {
			t.Errorf("%s 的 TotalKbps 漏了音轨: %d != %d + %d",
				tier.Name, tier.TotalKbps(), tier.VideoKbps, tier.AudioKbps)
		}
	}
}

// TestLowestTierFitsUpstream 把档位表和实测的上行预算对上一次，
// 顺带把"这套档位的现实含义"变成可执行断言。
//
// 12 Mbps 上行是我们实测的硬约束（bench-notes-video-delivery.md）。
// 最低档 480p/1128 kbps 必须留有余量 —— 否则"过载时只发最低档"
// 这个最后的退路本身就是超预算的，降级就完全没意义了。
func TestLowestTierFitsUpstream(t *testing.T) {
	const upstreamKbps = 12000 // 实测上行

	lowest := DefaultTiers[len(DefaultTiers)-1]
	if lowest.TotalKbps()*2 > upstreamKbps {
		t.Errorf("最低档 %s 是 %d kbps，连 2 个人都撑不住（上行 %d kbps）—— 降级退路失效",
			lowest.Name, lowest.TotalKbps(), upstreamKbps)
	}

	// 最高档必须**撑不住很多人**，否则这个扩展的前提就是假的：
	// 如果 1080p 档小到 12 Mbps 能喂饱一堆人，那就没有过载降级要解决的问题。
	highest := DefaultTiers[0]
	if highest.TotalKbps()*4 <= upstreamKbps {
		t.Errorf("最高档 %s 只有 %d kbps，上行 %d kbps 能喂 4 人以上 —— "+
			"请重新核对这个扩展的前提（bench-notes-video-delivery.md）",
			highest.Name, highest.TotalKbps(), upstreamKbps)
	}
}
