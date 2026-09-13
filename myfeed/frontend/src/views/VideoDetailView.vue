<template>
  <div class="page detail" :class="{ wide: isDesktop, docked: isDesktop && queue.hasActivity }">
    <!-- 移动端壳的顶栏没有返回键，而详情页是被"推进去"的一层，得自己给出口 -->
    <button v-if="!isDesktop" class="back" type="button" @click="goBack">
      <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true">
        <path
          d="M14.5 5.5 8 12l6.5 6.5"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
      返回
    </button>

    <p v-if="loading" class="card loading text-muted">打开视频…</p>

    <!-- 「不存在」和「没打开」分开：前者是确定的结论，后者可能只是后端没跑 -->
    <div v-else-if="notFound" class="empty-block">
      <p class="empty-title">这个视频不存在或已被删除</p>
      <p v-if="invalidID" class="text-muted">
        地址里的 <code>{{ rawID }}</code> 不是合法的视频 ID（要正整数），所以根本没发请求。
      </p>
      <p v-else class="text-muted">
        <code>POST /video/getDetail</code> 对 <code>{{ rawID }}</code> 返回了 404。
        也可能是发布者刚删了它 —— 删除只清数据库记录，磁盘上的 .mp4 其实还留着。
      </p>
      <RouterLink class="btn" to="/feed">回发现页</RouterLink>
    </div>

    <div v-else-if="error" class="empty-block">
      <p class="empty-title">没能打开这个视频</p>
      <p class="text-muted">{{ error }}</p>
      <button class="btn btn-ghost" type="button" @click="load">重试</button>
    </div>

    <div v-else-if="video" class="cols">
      <div class="main">
        <!-- :key 绑 play_url：从相关推荐跳到另一条时，强制整个播放器重建。
             旧元素卸载会走 onUnmounted 里的三步释放，解码器不会跟着漏 -->
        <VideoPlayer
          :key="video.play_url"
          :play-url="video.play_url"
          :cover-url="video.cover_url"
          :title="video.title"
        />

        <h1 class="title">{{ video.title }}</h1>

        <!-- 只摆真有的数据。B 站那排「播放量/弹幕数」我们没有对应字段：
             videos 表里只有 likes_count 和 popularity，而 popularity 是**热度分**、
             不是播放次数，标成"播放"就是编数据。 -->
        <p class="stats text-muted">
          <time class="mono" :title="fullTime(video.create_time)">{{ formatTime(video.create_time) }}</time>
          <span class="dot" aria-hidden="true">·</span>
          <span>{{ video.likes_count }} 点赞</span>
        </p>

        <ActionBar
          :likes-count="video.likes_count"
          :is-liked="isLiked"
          :liking="likeBusy"
          :is-owner="isOwner"
          :deleting="deleting"
          @toggle-like="onToggleLike"
          @delete="onDelete"
        />

        <!-- notice 既放删除/点赞的失败，也放"请先登录"。游客点点赞时给的是这里 -->
        <p v-if="notice" class="alert">
          {{ notice }}
          <RouterLink v-if="needLogin" class="alert-link" to="/login">去登录</RouterLink>
        </p>

        <div v-if="tags.length" class="tags">
          <RouterLink v-for="t in tags" :key="t" class="tag" :to="`/tag/${t}`">#{{ t }}</RouterLink>
        </div>

        <p v-if="video.description" class="desc">{{ video.description }}</p>

        <!-- 评论区（阶段5）。
             :key 绑 video.id —— 从相关推荐跳到另一条视频时视图是**复用**的，
             不重建的话新视频会先带着上一条的评论列表闪一下，然后才被替换掉。
             和播放器那边 :key="video.play_url" 是同一个理由 -->
        <CommentSection :key="video.id" :video-id="video.id" />
      </div>

      <aside class="side">
        <AuthorCard :author="author" :is-owner="isOwner" />
        <RelatedList :title="relatedTitle" :items="related" :loading="relatedLoading" />
      </aside>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import VideoPlayer from '../components/player/VideoPlayer.vue'
import ActionBar from '../components/video/ActionBar.vue'
import AuthorCard from '../components/video/AuthorCard.vue'
import CommentSection from '../components/video/CommentSection.vue'
import RelatedList from '../components/video/RelatedList.vue'
import { findByID, type AccountInfo } from '../api/account'
import { ApiError } from '../api/client'
import { listByTag, type FeedVideoItem } from '../api/feed'
import { isLiked as fetchIsLiked, likeVideo, unlikeVideo } from '../api/like'
import { deleteVideo, getDetail, listByAuthorID, normalizeVideo } from '../api/video'
import { useDevice } from '../composables/useDevice'
import { useAuthStore } from '../stores/auth'
import { useUploadQueueStore } from '../stores/uploadQueue'
import { extractTags } from '../utils/tags'
import { formatTime, fullTime } from '../utils/time'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const queue = useUploadQueueStore()
const { isDesktop } = useDevice()

/** 侧栏一列最多摆这么多。listByAuthorID 后端硬编码 LIMIT 200 且没有游标，
 *  照单全收会在一条 350px 的窄列里堆 200 个懒加载缩略图。 */
const MAX_RELATED = 15

const rawID = computed(() => String(route.params.id ?? ''))

const loading = ref(false)
const notFound = ref(false)
const invalidID = ref(false)
const error = ref('')
const video = ref<FeedVideoItem | null>(null)
const author = ref<AccountInfo | null>(null)
const related = ref<FeedVideoItem[]>([])
const relatedTitle = ref('相关推荐')
const relatedLoading = ref(false)
const deleting = ref(false)
const notice = ref('')
const needLogin = ref(false) // notice 是不是"请先登录"（决定要不要配一个登录链接）

// 点赞状态。**不在 FeedVideoItem 里**：/video/getDetail 返回的是 videos 表那一行，
// 而"我赞过没有"是 likes 表和"我"的交叉信息，videos 表存不了，得单独问一次后端。
const isLiked = ref(false)
const likeBusy = ref(false)

const tags = computed(() =>
  video.value ? extractTags(video.value.title, video.value.description) : [],
)
const isOwner = computed(() => !!video.value && auth.claims?.account_id === video.value.author.id)

async function load() {
  loading.value = true
  notFound.value = false
  invalidID.value = false
  error.value = ''
  notice.value = ''
  needLogin.value = false
  video.value = null
  author.value = null
  related.value = []
  relatedTitle.value = '相关推荐'
  // 必须清空：从 /video/1 点到 /video/2 时**视图是被复用的**，
  // 不清的话新视频会先带着上一条的"已赞"状态闪一下
  isLiked.value = false

  // route.params.id 是**字符串**。原样塞进 JSON 发给后端的话，`ID uint` 反序列化就失败，
  // ShouldBindJSON 报错 → 归类成 500（不是 404），页面上会挂一条 Go 的英文报错。
  // 所以先转数字；转不出来就在本地判"不存在"，一个请求都不发。
  const id = Number(rawID.value)
  if (!Number.isInteger(id) || id <= 0) {
    invalidID.value = true
    notFound.value = true
    loading.value = false
    return
  }

  try {
    const raw = await getDetail(id)
    video.value = normalizeVideo(raw)
    document.title = `${raw.title} · myfeed`
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) notFound.value = true
    else error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }

  if (video.value) {
    void loadSide()
    void loadLikeState(id)
  }
}

/**
 * 单独问一次"我赞过这个视频吗"。
 *
 * 游客**不发**这个请求：/like/* 挂的是 JWTAuth（不是 /feed 的 SoftJWTAuth），
 * 没 token 必然 401，而 client.ts 对任何 401 都会 clearTokens。
 * 游客本来就没 token，白跑一趟还平白多一次失败请求。
 */
async function loadLikeState(id: number) {
  if (!auth.isLoggedIn) {
    isLiked.value = false
    return
  }
  try {
    const res = await fetchIsLiked(id)
    // 快速连点相关推荐时上一轮可能后到 —— 认领结果前先确认还停在同一个视频上
    if (video.value?.id !== id) return
    isLiked.value = res.is_liked
  } catch {
    // 只读接口失败不该让整页报错：保持 false，用户点一下自然会知道真相
    // （那时 /like/like 会给出真正的错误）。这里不设 notice，也不打断播放。
    isLiked.value = false
  }
}

/**
 * 点赞 / 取消点赞。
 *
 * **乐观更新**：先改界面，再发请求。点赞是高频、低风险、结果几乎必然成功的动作，
 * 等一次往返（局域网 5ms、线上可能 200ms）才变色，会让每次点击都像卡了一下。
 * 代价是"猜错了必须能退回去"——所以下面**每一个**失败分支都先 rollback()。
 *
 * 这里也是"点赞状态归父组件所有"的体现：ActionBar 只报事件，
 * 计数和 isLiked 的真相都在这一个地方。
 */
async function onToggleLike() {
  const v = video.value
  if (!v || likeBusy.value) return

  // 游客：不跳走，就地给一句话 + 一个登录入口。
  // 正在看的视频可能已经播到一半，为了点赞把人踢到登录页是破坏性的。
  if (!auth.isLoggedIn) {
    notice.value = '登录后才能点赞。'
    needLogin.value = true
    return
  }

  const prevLiked = isLiked.value
  const prevCount = v.likes_count

  // 乐观更新。Math.max(0, ...) 是兜底：后端计数有 GREATEST(x-1, 0)，
  // 前端这边也不能显示成负数（本地状态万一偏了，界面不该跟着错）
  isLiked.value = !prevLiked
  v.likes_count = Math.max(0, prevCount + (prevLiked ? -1 : 1))

  // 写成箭头函数而不是 function 声明：函数声明会被提升，TS 在里面不放行
  // `const v` 上面那次 null 收窄（"v 在函数声明里可能在别处被调用"），
  // 于是报 'v' is possibly 'null'。箭头函数就地定义、收窄有效。
  const rollback = () => {
    isLiked.value = prevLiked
    v.likes_count = prevCount
  }

  likeBusy.value = true
  notice.value = ''
  needLogin.value = false
  try {
    if (prevLiked) await unlikeVideo(v.id)
    else await likeVideo(v.id)
  } catch (e) {
    rollback()
    if (e instanceof ApiError && e.status === 401) {
      // api/client.ts 的 handleResponse 对**任何** 401 都会 clearTokens。
      // token 过期时点个赞会静默把人登出，不解释一句用户完全不知道为什么
      notice.value = '登录状态已失效，请重新登录后再试。'
      needLogin.value = true
    } else if (e instanceof ApiError && e.status === 404) {
      notice.value = '这个视频不存在或已被删除（可能刚被作者删掉）。'
    } else {
      notice.value = e instanceof Error ? e.message : '操作失败'
    }
    // 乐观更新真正难的地方不是"请求失败"，而是"失败时本地状态**本来就是错的**"：
    // 比如同一个人在另一个标签页赞过了，我们这边还显示未赞 —— 这时仅回滚会把
    // 错误状态继续留着。业务错误（500）说明服务器和我们对同一件事有不同看法，
    // 所以再对一次账；网络类错误（status 0）服务器根本没表态，回滚就够了。
    if (e instanceof ApiError && e.status >= 500) void resyncLike(v.id)
  } finally {
    likeBusy.value = false
  }
}

/** 以后端为准，重新对齐 isLiked 和计数。只在乐观更新失败后调用 */
async function resyncLike(id: number) {
  try {
    const [state, raw] = await Promise.all([fetchIsLiked(id), getDetail(id)])
    if (video.value?.id !== id) return
    isLiked.value = state.is_liked
    // 计数以 getDetail 为准：like/unlike 的响应里只有一句 message，没有计数
    video.value.likes_count = raw.likes_count
  } catch {
    /* 对账也失败就保持回滚后的值 —— 用户还能再点一次，不该弹第二个错误 */
  }
}

/**
 * 作者卡 + 相关推荐。刻意和主体分开、不 await：正文（播放器）先出来，
 * 侧栏晚一两百毫秒到无所谓，卡着播放器不出来才是问题。
 */
async function loadSide() {
  const v = video.value
  if (!v) return

  const anchor = tags.value[0]
  relatedLoading.value = true

  // 两条腿都是现成的公开接口：
  //   有 #标签 → /feed/listByTag，标题写「相关推荐」
  //   没有标签 → /video/listByAuthorID，标题写「TA 的其它作品」
  // 标题跟着数据源变：这两条腿给出的确实不是一回事，都叫"相关推荐"是在骗人。
  // allSettled 而不是 all：作者接口挂了不该把相关推荐一起带走。
  const [a, rel] = await Promise.allSettled([
    findByID(v.author.id),
    anchor ? listByTag(anchor, 20) : listByAuthorID(v.author.id),
  ] as const)

  // 快速连点相关推荐时，上一轮可能后到 —— 认领结果前先确认还停在同一个视频上
  if (video.value?.id !== v.id) return

  if (a.status === 'fulfilled') author.value = a.value

  if (rel.status === 'fulfilled') {
    const list = Array.isArray(rel.value) ? rel.value.map(normalizeVideo) : rel.value.video_list
    relatedTitle.value = anchor ? '相关推荐' : 'TA 的其它作品'
    related.value = list.filter((x) => x.id !== v.id).slice(0, MAX_RELATED)
  }

  relatedLoading.value = false
}

onMounted(load)

// 路由参数变了但视图被**复用**（点相关推荐就是 /video/1 → /video/2）。
// 必须重新拉数据 + 滚回顶部，否则会出现"地址变了、画面没变"。
// TagView 对 /tag/:name 就是同一套写法。
watch(
  () => route.params.id,
  () => {
    window.scrollTo(0, 0)
    void load()
  },
)

const prevTitle = document.title
onUnmounted(() => {
  document.title = prevTitle
})

function goBack() {
  // 有历史才 back；直接刷新 /video/5 进来的话退无可退，回发现页
  if (window.history.state?.back) router.back()
  else router.replace('/feed')
}

async function onDelete() {
  const v = video.value
  if (!v) return
  if (!window.confirm(`确认删除「${v.title}」？\n\n只会删数据库记录，磁盘上的 .mp4 文件保留。`)) return

  deleting.value = true
  notice.value = ''
  needLogin.value = false
  try {
    await deleteVideo(v.id)
    // replace 而不是 push：push 会在历史里留下 /video/:id，按返回又落回刚删掉的那一页
    router.replace('/feed')
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      // api/client.ts 的 handleResponse 对**任何** 401 都会 clearTokens。
      // 不是本人 / token 过期时删除会 401，不解释一句的话，
      // 用户只会发现自己莫名其妙被登出了，完全不知道是刚才那一下删除导致的
      notice.value = '登录状态已失效，请重新登录后再试。'
      needLogin.value = true
    } else {
      notice.value = e instanceof Error ? e.message : '删除失败'
    }
  } finally {
    deleting.value = false
  }
}
</script>

<style scoped>
.detail {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.back {
  align-self: flex-start;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: 40px;
  padding: 0 12px 0 8px;
  border: none;
  border-radius: 10px;
  background: transparent;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.86rem;
  font-weight: 600;
  cursor: pointer;
  transition:
    color 0.18s var(--ease-out),
    background 0.18s var(--ease-out);
}
.back:hover {
  color: var(--ink);
  background: var(--surface-2);
}

.loading {
  margin: 0;
  padding: 22px;
  border-radius: 18px;
  font-size: 0.9rem;
}

/* 单列是默认，两列在下面 ≥1080px 的媒体查询里打开 */
.cols {
  display: grid;
  grid-template-columns: 1fr;
  gap: 22px;
}

.main {
  display: flex;
  flex-direction: column;
  gap: 14px;
  min-width: 0;
}

.title {
  margin: 0;
  font-size: 1.28rem;
  line-height: 1.45;
  letter-spacing: -0.02em;
  /* 不 clamp：详情页的标题要能整条读到，而且得能被选中复制 */
  overflow-wrap: anywhere;
}

.stats {
  display: flex;
  align-items: center;
  gap: 7px;
  margin: 0;
  font-size: 0.8rem;
}
.dot {
  opacity: 0.5;
}

.tags {
  display: flex;
  flex-wrap: wrap;
  gap: 7px;
}
.tag {
  padding: 3px 10px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--surface-2);
  color: var(--ink-muted);
  font-size: 0.78rem;
}
.tag:hover {
  color: var(--accent);
  border-color: var(--accent-line);
}

.desc {
  margin: 0;
  padding: 14px 16px;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface);
  font-size: 0.88rem;
  line-height: 1.7;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

/* 评论区的样式全在 CommentSection.vue 里（scoped）。
   这里原本有 .comments/.cinput/.soon/.note 四条规则，随"评论区占位"一起删了 ——
   .soon（阶段角标）是这个文件里最后一处使用，它退休了；阶段7+ 再加
   "未接入"角标时，那套样式应该在新的组件里重新写，而不是在这里留一份无人引用的 */

.alert-link {
  margin-left: 6px;
  color: currentColor;
  text-decoration: underline;
}

.side {
  display: flex;
  flex-direction: column;
  gap: 14px;
  min-width: 0;
}

@media (max-width: 767px) {
  .detail {
    gap: 12px;
  }
  .title {
    font-size: 1.1rem;
  }
  /* 播放器贴边。MobileShell 的 .content 左右各留 12px，而 .page 按约定不管内边距
     （留白是壳的职责），所以这里用负 margin 把播放器顶到屏幕边上 —— B 站手机端就是这样。
     用 :deep 是因为 .player 是子组件的根元素，要压过它自己的 border-radius */
  .main :deep(.player) {
    margin-inline: -12px;
    border-radius: 0;
    border-left: 0;
    border-right: 0;
  }
}

/*
  两列布局从 1080px 才开始，不是 768px。
  算一下就知道：768px 视口下 .page 只有 768-48=720px，扣掉 350px 侧栏和 26px 间距
  只剩 344px 给播放器 —— 侧栏比播放器还宽。B 站收窄到那个程度也是改成上下堆叠的。
  768~1079px 之间我们保持单列，作者卡和相关推荐落到播放器下面。
*/
@media (min-width: 1080px) {
  .wide .cols {
    grid-template-columns: minmax(0, 1fr) 350px;
    gap: 26px;
    /* 侧栏是 grid item，默认 align-items: stretch 会把它拉到和主列一样高，
       那样它一点可滚动余量都没有，sticky 等于没写 */
    align-items: start;
  }
  .wide .side {
    position: sticky;
    /* 顶栏(--nav-h) + 频道条(--channel-h) 都是 sticky 的，再留 24px 呼吸位 */
    top: calc(var(--nav-h) + var(--channel-h) + 24px);
  }
  /* 上传浮窗（App.vue 里那层 fixed、宽 320px、贴右下角）正好压住这一列。
     有上传在跑的时候取消 sticky 并把底部垫高，滚动时侧栏内容能整个躲开浮窗 */
  .wide.docked .side {
    position: static;
    padding-bottom: 220px;
  }
}
</style>
