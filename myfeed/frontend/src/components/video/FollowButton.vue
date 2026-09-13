<template>
  <!-- v-if="!isMine" 在模板根上：自己的主页/自己的作品下面不该有"关注自己" -->
  <template v-if="!isMine">
    <button
      class="follow"
      :class="{ on: following }"
      type="button"
      :disabled="busy || loading"
      :aria-pressed="following"
      :title="title"
      @click="onToggle"
    >
      {{ label }}
    </button>

    <p v-if="notice" class="fnotice">
      {{ notice }}
      <RouterLink v-if="needLogin" class="link-accent" to="/login">去登录</RouterLink>
    </p>
  </template>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'

import { ApiError } from '../../api/client'
import { follow, getAllVloggers, unfollow } from '../../api/social'
import { invalidateFeedStream } from '../../composables/useFeedStream'
import { useAuthStore } from '../../stores/auth'

/**
 * 关注 / 取关按钮。详情页的作者卡和个人主页共用**同一个组件**，
 * 因为"关注某个人"是一个自足的状态机（查状态 → 乐观切换 → 对账 → 失效关注流），
 * 抄第二份必然只抄到一半。
 *
 * ================== 一个必须说清的后端空缺 ==================
 *
 * **没有 isFollowed 接口。** 后端 /social 下只有 follow / unfollow /
 * getAllFollowers / getAllVloggers / getCounts —— 原项目的 SocialService 里
 * 也没有 IsFollowed（因为原项目的前端压根没做这个按钮）。
 *
 * 所以"我关注他了吗"只能**倒推**：拉我自己的关注列表（getAllVloggers 传 0 =
 * 查自己），看这个 id 在不在里面。这个方案有两个已知的、真会错的边界：
 *
 *   ① 关注超过 **200 人**时，后端 LIMIT 200 会把第 201 个之后的人截掉，
 *      于是那些人在按钮上显示成「关注」（其实已关注），点下去后端返回
 *      "already followed" → 前端重对账 → 还是显示「关注」。**按钮会永远错下去。**
 *   ② 每挂一个按钮就多一次请求，而且传回来的是整个列表
 *      （只要一个布尔值，却搬了 O(关注数) 的数据）。
 *
 * 真正的修法是后端补一个 `isFollowed(vlogger_id)`（一条 COUNT 的事）。
 * 这里不偷偷绕过去，就把这两个代价写在按钮旁边。
 *
 * ================== 失败重对账 ==================
 *
 * 500 这个状态码在本项目的 /social 下**同时表示**"重复关注/未关注就取关"
 * （裸 error 被 ClassifyHTTPStatus 兜成 500）和"服务器真炸了"。
 * 分不开，所以失败后不猜，直接重新拉一次状态 —— 和 VideoDetailView 里
 * resyncLike 是同一个模式：**乐观更新的失败分支，服务器才是真相**。
 */
const props = defineProps<{
  vloggerId: number
  /** 是不是我自己。是的话整个按钮不渲染 */
  isOwner?: boolean
}>()

const auth = useAuthStore()

const following = ref(false)
const loading = ref(false)
const busy = ref(false)
const notice = ref('')
const needLogin = ref(false)

const isMine = computed(
  () => props.isOwner === true || auth.claims?.account_id === props.vloggerId,
)

const label = computed(() => (following.value ? '已关注' : '关注'))
const title = computed(() => {
  if (loading.value) return '正在确认关注状态…'
  if (busy.value) return '处理中…'
  if (!auth.isLoggedIn) return '登录后才能关注'
  return following.value ? '点击取消关注' : '点击关注'
})

async function loadState() {
  if (props.vloggerId <= 0 || isMine.value) return
  // 游客不发这个请求：/social/* 整组强鉴权，必然 401，
  // 而 client.ts 对任何 401 都会 clearTokens —— 白跑一趟还多一次失败记录
  if (!auth.isLoggedIn) {
    following.value = false
    return
  }
  loading.value = true
  try {
    const res = await getAllVloggers(0)
    if (!res.vloggers) return // 后端理论上不会给 null，真给了就当查不到
    following.value = res.vloggers.some((v) => v.id === props.vloggerId)
  } catch {
    // 查不到就按"未关注"渲染。这不是撒谎：点一下就会知道真相，
    // 而且后端本来就有 1062 那条唯一索引兜着（重复关注写不进去）。
    // 不设 notice —— 一个只读的状态查询失败，不该在页面上先报一个错。
    following.value = false
  } finally {
    loading.value = false
  }
}

onMounted(loadState)
// 从 /video/1 点到 /video/2（视图复用）时作者变了，状态必须跟着重查
watch(() => props.vloggerId, loadState)

async function onToggle() {
  const id = props.vloggerId
  if (id <= 0 || busy.value) return

  if (!auth.isLoggedIn) {
    notice.value = '登录后才能关注。'
    needLogin.value = true
    return
  }

  const prev = following.value

  // 乐观更新：关注是低频、低风险、结果几乎必然成功的动作，
  // 等一次往返才变色会让点击看起来像卡了一下
  following.value = !prev

  // 写成箭头函数：TS 在函数声明里不放行上面那次 !prev 的收窄（同 VideoDetailView）
  const rollback = () => {
    following.value = prev
  }

  busy.value = true
  notice.value = ''
  needLogin.value = false
  try {
    if (prev) await unfollow(id)
    else await follow(id)
    // 关注关系变了 → 内存里的「关注流」就过期了。
    // 不作废的话，用户关注完切到「关注」tab 看到的还是关注之前拉的那一页。
    // 这是**前端版的缓存失效**；阶段7 会在后端做同一件事
    // （invalidateFollowingFeedCache），只是失效对象换成 Redis 里的那份。
    invalidateFeedStream('following')
  } catch (e) {
    rollback()
    if (e instanceof ApiError && e.status === 401) {
      // handleResponse 对任何 401 都 clearTokens（同点赞、删除视频那两处）
      notice.value = '登录状态已失效，请重新登录后再试。'
      needLogin.value = true
    } else if (e instanceof ApiError && e.status >= 500) {
      notice.value = prev
        ? '取关没成功 —— 可能你本来就没关注他。'
        : '关注没成功 —— 可能你本来就关注了。'
      void loadState()
    } else {
      notice.value = e instanceof Error ? e.message : '操作失败'
    }
  } finally {
    busy.value = false
  }
}
</script>

<style scoped>
.follow {
  align-self: flex-start;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 7px;
  min-height: 38px;
  padding: 0 16px;
  border: 1px solid var(--border);
  border-radius: 11px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.88rem;
  font-weight: 600;
  cursor: pointer;
  transition:
    background 0.15s,
    border-color 0.15s,
    color 0.15s;
}
.follow:hover:not(:disabled) {
  border-color: var(--border-strong);
}
/* 已关注：用柔和的主色底表达"这是一条既成事实"，而不是把它做成一个危险按钮。
   取关是能再点回来的，不需要红色警报 */
.follow.on {
  border-color: var(--accent-line);
  background: var(--accent-soft);
  color: var(--accent);
}

.fnotice {
  margin: 0;
  font-size: 0.76rem;
  line-height: 1.5;
  color: var(--danger);
}
</style>
