# Design System: myfeed — 短视频 Feed 工作台

## 1. Visual Theme & Atmosphere

深夜剪辑室（midnight edit bay）气质：Density 5 / Variance 5 / Motion 6。
暗色但不是黑的——Canvas 是带一点暖意的炭灰，唯一的高频色彩是赤陶橙（Terracotta），
用在关键动作与激活态上，克制而确定。界面像一个安静的创作工具，不是一个霓虹游乐场。

## 2. Color Palette & Roles

- **Canvas Charcoal** (#0F0F12) — 全局背景。禁纯黑 #000000。
- **Surface Slate** (#17171C) — 卡片/面板填充
- **Surface Raised** (#1D1D24) — 悬浮层、输入框底
- **Ink Bright** (#F2F2F0) — 主文本
- **Ink Muted** (#9B9BA6) — 次级文本、元信息
- **Whisper Border** (rgba(255,255,255,0.08)) — 1px 结构线、卡片描边
- **Terracotta Signal** (#E8674A, hover #F07B5F) — 唯一强调色：主按钮、激活态、焦点环。HSL 饱和度 ~77%
- **State OK** (#4AB784) / **State Danger** (#E5534B) — 仅用于状态反馈，不作装饰
- 阴影统一染底色：`0 20px 48px rgba(0,0,0,0.35)`。禁外发光/霓虹 glow

## 3. Typography Rules

- **Display/Body 拉丁字符**：Outfit（@fontsource 本地打包——Google Fonts 国内不可达，刻意本地化）
- **Mono**：JetBrains Mono — ID、时间戳、token 等技术数据
- **中文回退**：Noto Sans SC → Microsoft YaHei（系统栈，刻意声明）
- 层级靠 **字重（500/600/700）与颜色**，不靠巨字号；标题 letter-spacing -0.02em
- 正文行高 1.6，段落 max-width 65ch
- 禁：Inter、Times/Georgia 类衬线（软件 UI 永远无衬线）

## 4. Component Stylings

- **Buttons**：平面填充，无外发光。active 时 `translateY(-1px)`→按压回落（触觉感）。
  主按钮 Terracotta 填充 + Ink 白字；次按钮透明底 + Whisper 边框
- **Cards**：24px 圆角，1px Whisper 边框 + 染色阴影，仅在有层级意义时用卡片
- **Inputs**：label 在上、错误在下；focus 时 2px Terracotta 焦点环（外扩 2px，无 glow）
- **Loaders**：骨架屏 shimmer（与布局同形），禁圆形转圈
- **空态**：构图式提示（图标+一句话+下一步动作），不写干巴巴的"无数据"

## 5. Layout Principles

- Grid 优先，max-width 1080px 居中收拢；左右留白 ≥24px
- 禁三等分卡片横排——用不对称两栏或纵向流
- 元素不重叠，各占各的空间；禁绝对定位叠放
- 全高区块用 `min-height:100dvh`，禁 `h-screen`
- <768px 一律单列折叠；触控目标 ≥44px

## 6. Motion & Interaction

- 缓动：出场用 spring 手感 `cubic-bezier(0.34,1.56,0.64,1)`，过渡用 `cubic-bezier(0.22,1,0.36,1)`
- 只动画 `transform` 和 `opacity`，禁 top/left/width/height
- 列表/表单元素错峰入场（stagger 60ms 递增）
- 永续微动效：品牌点的呼吸（2.4s 循环）——全站仅此一处常驻动画
- `prefers-reduced-motion` 时全部动效归零

## 7. Anti-Patterns（本项目明确禁止）

- 禁 emoji 进 UI、禁紫色/霓虹渐变、禁纯黑背景
- 禁 Inter 字体、禁衬线体、禁外发光阴影
- 禁"Scroll to explore"类填充文案、禁 AI 腔文案（赋能/无缝/极致）
- 禁编造数据（"99.9% 可用率"这类假指标一律不出现）
- 禁居中对称的 Hero（本项目 Variance=5，用左对齐+不对称留白）
- 禁三个等宽卡片一排的模板布局
