<template>
  <div class="page">
    <!-- 和详情页/点赞页同理：移动端壳的顶栏没有返回键 -->
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

    <p v-if="loading" class="card loading text-muted">打开主页…</p>

    <!-- 「不存在」和「没打开」分开，和视频详情页同一套说法 -->
    <div v-else-if="notFound" class="empty-block">
      <p class="empty-title">没有这个账号</p>
      <p v-if="invalidID" class="text-muted">
        地址里的 <code>{{ rawID }}</code> 不是合法的账号 ID（要正整数），所以根本没发请求。
      </p>
      <p v-else class="text-muted">
        <code>POST /account/getProfile</code> 对 <code>{{ rawID }}</code> 没有找到账号。
      </p>
      <p class="text-muted mono small">
        注：后端现在是显式 500，要让这一屏真的出现，得把 profile/handler.go 里那句
        改成 apierror.ClassifyHTTPStatus —— 见那里和这里的注释。
      </p>
      <RouterLink class="btn" to="/feed">回发现页</RouterLink>
    </div>

    <div v-else-if="error" class="empty-block">
      <p class="empty-title">没能打开这个主页</p>
      <p class="text-muted">{{ error }}</p>
      <button class="btn btn-ghost" type="button" @click="load">重试</button>
    </div>

    <template v-else-if="profile">
      <!-- ---------- 头部 ---------- -->
      <header class="card head">
        <span class="avatar" aria-hidden="true">
          <img v-if="avatarURL" :src="avatarURL" alt="" />
          <template v-else>{{ initial }}</template>
        </span>

        <div class="who">
          <h1>{{ profile.account.username }}</h1>
          <p class="sub mono text-muted">UID {{ profile.account.id }}</p>
          <p v-if="profile.account.bio" class="bio">{{ profile.account.bio }}</p>
          <p v-else class="bio text-muted">这个账号还没写简介。</p>

          <!-- 这是唯一一个真能用它的地方：/account/getProfile 是公开的，
               游客也能看别人的主页，所以「关注」按钮对游客是"点了提示登录"
               而不是藏起来（按钮自己处理这件事，见 FollowButton.vue） -->
          <div class="actions">
            <p v-if="isSelf" class="self text-muted">这是你自己的主页。</p>
            <FollowButton v-else :vlogger-id="profile.account.id" />
          </div>
        </div>

        <!-- 四个计数。全部来自那一次 getProfile：
             作品/总获赞 是 videos 表的，粉丝/关注 是 socials 表的 ——
             一次请求读三个模块，这就是 profile 包存在的理由 -->
        <dl class="stats">
          <div><dt>作品</dt><dd class="mono">{{ profile.video_count }}</dd></div>
          <div><dt>获赞</dt><dd class="mono">{{ profile.total_likes }}</dd></div>
          <div><dt>粉丝</dt><dd class="mono">{{ profile.follower_count }}</dd></div>
          <div><dt>关注</dt><dd class="mono">{{ profile.vlogger_count }}</dd></div>
        </dl>
      </header>

      <!-- ---------- 作品 ---------- -->
      <div class="head">
        <div class="head-left">
          <h2>作品</h2>
          <p class="lead mono">POST /video/listByAuthorID · 公开 · 后端 LIMIT 200 且无游标</p>
        </div>
        <button class="btn btn-ghost sm" type="button" :disabled="videosLoading" @click="loadVideos">
          刷新
        </button>
      </div>

      <p v-if="videosError" class="alert">{{ videosError }}</p>

      <component
        :is="Grid"
        :items="videos"
        :loading="videosLoading"
        :loaded="videosLoaded"
        :has-more="false"
        :error="videosError"
      >
        <template #empty>
          <svg viewBox="0 0 64 64" width="52" height="52" aria-hidden="true" class="empty-icon">
            <rect x="8" y="16" width="48" height="34" rx="5" fill="none" stroke="currentColor" stroke-width="2" />
            <path d="M26 27.5v11l10-5.5z" fill="currentColor" />
            <path d="M20 10h24" stroke="currentColor" stroke-width="2" stroke-linecap="round" opacity="0.45" />
          </svg>
          <p class="empty-title">TA 还没发过作品</p>
          <p class="text-muted">关注一下，等 TA 更新。</p>
          <RouterLink class="btn" to="/feed">去发现页</RouterLink>
        </template>
      </component>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import DesktopVideoGrid from '../components/desktop/VideoGrid.vue'
import MobileVideoGrid from '../components/mobile/VideoGrid.vue'
import FollowButton from '../components/video/FollowButton.vue'
import { getProfile, type ProfileResult } from '../api/account'
import { ApiError } from '../api/client'
import type { FeedVideoItem } from '../api/feed'
import { listByAuthorID, normalizeVideo, staticURL } from '../api/video'
import { useDevice } from '../composables/useDevice'
import { useAuthStore } from '../stores/auth'

/**
 * 个人主页（阶段6）。
 *
 * 路由 `/profile/:id`，**不加 requiresAuth**：背后是 `/account/getProfile`，
 * 一个公开接口 —— 看别人的主页不需要登录。（复制 `/likes` 那条路由进来时
 * 特别容易把 meta 一起抄过去，抄了游客就点不进任何人的主页了。）
 *
 * 这一页的数据是**两个独立接口**拼的，刻意不合成一个：
 *
 *   /account/getProfile   账号 + 四个计数（跨模块聚合，在 Go 里是 internal/profile 包）
 *   /video/listByAuthorID 作品列表（videos 表，公开）
 *
 * 为什么不把作品列表也塞进 getProfile：那样 profile 包就要返回 video 的形状，
 * 而它现在只返回**计数**（int64），对 video 包只有一个 repo 方法的依赖。
 * 一旦把列表塞进去，account 模块的响应里就长出了 videos 的字段形状 ——
 * "聚合"就变成了"重新定义别的模块的数据"。
 */
const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const { isDesktop } = useDevice()

const Grid = computed(() => (isDesktop.value ? DesktopVideoGrid : MobileVideoGrid))

const rawID = computed(() => String(route.params.id ?? ''))

const profile = ref<ProfileResult | null>(null)
const loading = ref(false)
const notFound = ref(false)
const invalidID = ref(false)
const error = ref('')

const videos = ref<FeedVideoItem[]>([])
const videosLoading = ref(false)
const videosLoaded = ref(false)
const videosError = ref('')

const isSelf = computed(() => !!profile.value && auth.claims?.account_id === profile.value.account.id)
const initial = computed(() => profile.value?.account.username?.slice(0, 1).toUpperCase() ?? '?')
const avatarURL = computed(() =>
  profile.value?.account.avatar_url ? staticURL(profile.value.account.avatar_url) : '',
)

async function load() {
  loading.value = true
  notFound.value = false
  invalidID.value = false
  error.value = ''
  profile.value = null
  videos.value = []
  videosLoaded.value = false
  videosError.value = ''

  // route.params.id 是字符串。原样发给后端，`AccountID uint` 反序列化就失败，
  // ShouldBindJSON 报错 → 500（不是 404），页面上会挂一条 Go 的英文报错。
  // 所以先在本地转数字，转不出来就判"不存在"，一个请求都不发（同 VideoDetailView）
  const id = Number(rawID.value)
  if (!Number.isInteger(id) || id <= 0) {
    invalidID.value = true
    notFound.value = true
    loading.value = false
    return
  }

  try {
    profile.value = await getProfile(id)
    document.title = `${profile.value.account.username} · myfeed`
  } catch (e) {
    // **404 这一支今天是打不中的**，留着是因为后端离它只有一行。
    //
    // profile/handler.go 里账号不存在时走的是**显式 500**（刻意保留原项目的写法），
    // 而不是 ClassifyHTTPStatus —— 后者会把 gorm.ErrRecordNotFound 翻译成 404。
    // 也就是说：现在访问一个不存在的 ID，用户看到的是 error 分支里那句
    // Go 英文报错，不是体面的"没有这个账号"。
    //
    // 不在这里去猜 message 里有没有 "record not found"：那是在用字符串匹配
    // 补一个本该由状态码表达的语义，后端改一次错误文案前端就悄悄失效。
    // 该改的是后端那一行。
    if (e instanceof ApiError && e.status === 404) {
      notFound.value = true
    } else if (e instanceof ApiError && e.status >= 500) {
      error.value = `${e.message}（账号可能不存在，也可能是后端出错了 —— 这一步本该返回 404）`
    } else {
      error.value = e instanceof Error ? e.message : String(e)
    }
  } finally {
    loading.value = false
  }

  if (profile.value) void loadVideos()
}

/** 作品列表：刻意和主体分开、不 await —— 主页头部先出来，
 *  网格晚一两百毫秒到无所谓（和详情页的 loadSide 同一个取舍） */
async function loadVideos() {
  const id = profile.value?.account.id
  if (!id) return
  videosLoading.value = true
  videosError.value = ''
  try {
    const raw = await listByAuthorID(id)
    // 快速连点/切页时上一轮可能后到 —— 认领结果前先确认还停在同一个主页上
    if (profile.value?.account.id !== id) return
    // 返回的是 **videos 表的原始形状**（扁平 username + RFC3339 时间），
    // 不是 FeedVideoItem —— 必须过 normalizeVideo，否则时间会渲染成 Invalid Date
    videos.value = raw.map(normalizeVideo)
  } catch (e) {
    if (profile.value?.account.id !== id) return
    videosError.value = e instanceof Error ? e.message : '加载作品失败'
  } finally {
    videosLoading.value = false
    // 失败时置 true 会让网格显示"TA 还没发过作品"—— 那是在撒谎
    videosLoaded.value = !videosError.value
  }
}

onMounted(load)

// 路由参数变了但视图被复用（从一个人的主页点进另一个人的）。必须重新拉数据 +
// 滚回顶部，否则会出现"地址变了、画面没变"。TagView / VideoDetailView 同一套写法
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
  if (window.history.state?.back) router.back()
  else router.replace('/feed')
}
</script>

<style scoped>
/* .page 不是 flex 容器（就是 max-width + margin auto），所以这里不用 align-self */
.back {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: 40px;
  margin-bottom: 6px;
  padding: 0 12px 0 8px;
  border: none;
  border-radius: 10px;
  background: transparent;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.86rem;
  font-weight: 600;
  cursor: pointer;
}
.back:hover {
  color: var(--ink);
  background: var(--surface-2);
}

.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 18px;
}
.head-left {
  min-width: 0;
}
h2 {
  margin: 0 0 4px;
  font-size: 1.05rem;
  letter-spacing: -0.02em;
}
.lead {
  margin: 0;
  font-size: 0.72rem;
  color: var(--ink-muted);
  overflow-wrap: anywhere;
}

/* ---------- 资料卡 ---------- */
header.head {
  align-items: center;
  flex-wrap: wrap;
  padding: 18px;
  border-radius: 18px;
}
.avatar {
  flex: 0 0 auto;
  width: 64px;
  height: 64px;
  border-radius: 50%;
  overflow: hidden;
  display: grid;
  place-items: center;
  background: var(--surface-2);
  border: 1px solid var(--accent-line);
  color: var(--accent);
  font-weight: 700;
  font-size: 1.4rem;
}
.avatar img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.who {
  flex: 1 1 220px;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 5px;
}
h1 {
  margin: 0;
  font-size: 1.3rem;
  letter-spacing: -0.02em;
  overflow-wrap: anywhere;
}
.sub {
  margin: 0;
  font-size: 0.74rem;
}
.bio {
  margin: 0;
  font-size: 0.85rem;
  line-height: 1.6;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.actions {
  margin-top: 4px;
}
.self {
  margin: 0;
  font-size: 0.8rem;
}

/* ---------- 四个计数 ---------- */
.stats {
  flex: 1 1 100%;
  display: flex;
  flex-wrap: wrap;
  gap: 22px;
  margin: 16px 0 0;
  padding-top: 14px;
  border-top: 1px solid var(--border);
}
.stats div {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.stats dt {
  color: var(--ink-muted);
  font-size: 0.72rem;
}
.stats dd {
  margin: 0;
  font-size: 1.05rem;
  font-weight: 700;
}

.small {
  font-size: 0.74rem;
}

.empty-icon {
  color: var(--ink-muted);
  opacity: 0.7;
}

@media (max-width: 767px) {
  h1 {
    font-size: 1.1rem;
  }
  .lead {
    display: none; /* 手机上接口路径是噪音 */
  }
  .stats {
    gap: 18px;
  }
}
</style>
