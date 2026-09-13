<template>
  <section class="card comments">
    <h2>
      评论
      <span v-if="loaded" class="count mono">{{ comments.length }}</span>
      <span v-if="!auth.isLoggedIn" class="soon">游客只读</span>
      <span v-if="capsAt200" class="soon" title="后端 LIMIT 200，没有游标">只显示前 200 条</span>
      <!-- 刷新按钮是必需的，不只是方便：删除失败（403/500）之后列表可能是过期的，
           而"重新加载"是唯一的补救动作 —— 提示里说要刷新，就得有一个能刷新的地方 -->
      <button class="reload" type="button" :disabled="loading" @click="load">
        {{ loading && loaded ? '刷新中…' : '刷新' }}
      </button>
    </h2>

    <!-- ---------- 发布区 ---------- -->
    <template v-if="auth.isLoggedIn">
      <textarea
        v-model="draft"
        class="cinput"
        rows="3"
        :disabled="publishing"
        placeholder="说点什么…  用 @用户名 提到某人，后端会给他写一条通知"
        @keydown="onKeydown"
      ></textarea>
      <div class="compose">
        <button class="btn" type="button" :disabled="publishing || !canPublish" @click="onPublish">
          {{ publishing ? '发布中…' : '发布' }}
        </button>
        <span class="hint text-muted mono">Ctrl/⌘ + Enter 也能发 · 身份只从 token 取</span>
      </div>
    </template>
    <p v-else class="guestline text-muted">
      看评论不需要登录，发评论需要。
      <RouterLink class="link-accent" to="/login">去登录</RouterLink>
    </p>

    <p v-if="notice" class="alert">
      {{ notice }}
      <RouterLink v-if="needLogin" class="alert-link" to="/login">去登录</RouterLink>
    </p>

    <!-- ---------- 列表 ---------- -->
    <p v-if="loading && !loaded" class="text-muted sm">加载评论…</p>

    <template v-else-if="error">
      <p class="text-muted sm">{{ error }}</p>
      <button class="btn btn-ghost sm" type="button" @click="load">重试</button>
    </template>

    <p v-else-if="!comments.length" class="text-muted sm">
      还没有评论。第一条会出现在这里。
    </p>

    <ul v-else class="list">
      <li v-for="c in comments" :key="c.id" class="row">
        <span class="cavatar" aria-hidden="true">
          {{ c.username.slice(0, 1).toUpperCase() || '?' }}
        </span>

        <div class="body">
          <p class="meta">
            <!-- 名字链到个人主页（/account/getProfile 是公开接口，游客也点得动）。
                 这里显示的是 comments.username —— **发评论那一刻**的快照，
                 改名后不会跟着变；主页上那个才是当前值 -->
            <RouterLink class="uname" :to="`/profile/${c.author_id}`">{{ c.username }}</RouterLink>
            <span class="uid mono text-muted">UID {{ c.author_id }}</span>
            <time class="mono text-muted" :title="fullTime(isoToUnixSeconds(c.created_at))">
              {{ formatTime(isoToUnixSeconds(c.created_at)) }}
            </time>
          </p>
          <p class="text">{{ c.content }}</p>
        </div>

        <!-- 只给自己的评论渲染删除键。
             后端仍然会独立判一次权限（403）—— 前端这层是**减噪**，不是安全边界。
             谁都能改 DOM，真正的判断在 service.Delete 里。 -->
        <button
          v-if="isMine(c)"
          class="del"
          type="button"
          :disabled="busyId === c.id"
          title="删除这条评论（同时把视频热度 -1）"
          @click="onDelete(c)"
        >
          {{ busyId === c.id ? '删除中…' : '删除' }}
        </button>
      </li>
    </ul>

    <p class="note text-muted">
      评论是 <code>POST /comment/listAll</code>（公开），发布和删除都要登录。
      发布走一个事务：写 comments + <code>videos.popularity + 1</code>；
      删除反向减 1 —— <strong>这一对加减是本项目唯一手工维护的双向一致性</strong>，
      也是点赞（阶段4）没有写 popularity 的原因。
      @提及会在事务**提交之后**写一条 notifications 记录。
    </p>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'

import { ApiError } from '../../api/client'
import { deleteComment, listAllComments, publishComment, type CommentItem } from '../../api/comment'
import { useAuthStore } from '../../stores/auth'
import { formatTime, fullTime, isoToUnixSeconds } from '../../utils/time'

const props = defineProps<{ videoId: number }>()

const auth = useAuthStore()

const comments = ref<CommentItem[]>([])
const loading = ref(false)
const loaded = ref(false)
const error = ref('')

const draft = ref('')
const publishing = ref(false)

/** 正在删除的那一条的 id。用 id 而不是布尔，是为了只把**那一个**按钮置灰 */
const busyId = ref<number | null>(null)

const notice = ref('')
const needLogin = ref(false)

const canPublish = computed(() => draft.value.trim().length > 0)

/**
 * Ctrl+Enter / ⌘+Enter 发送。
 *
 * 没用 `@keydown.ctrl.enter` 这种修饰符链：Vue 3 里 `.ctrl` 和 `.meta` 是
 * **各自独立**的修饰符，绑两个会变成"两个键都得按"（keydown.ctrl.meta.enter）。
 * 想表达"任选其一"只能自己判 —— 这也是同一段代码在 Vue 2 和 3 里行为不同的地方。
 */
function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault() // 别在 textarea 里插一个换行
    void onPublish()
  }
}

/** 后端 LIMIT 200 且没有游标：拿到正好 200 条 = 大概率被截断了。
 *  这一页是**唯一**能提示这件事的信号，因为响应里没有总数。 */
const capsAt200 = computed(() => loaded.value && comments.value.length >= 200)

function isMine(c: CommentItem): boolean {
  return auth.isLoggedIn && c.author_id === auth.claims?.account_id
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    comments.value = await listAllComments(props.videoId)
    loaded.value = true
  } catch (e) {
    // 这是公开接口，401 理论上不该出现；真出现了也照实说，不冒充"没有评论"
    error.value = e instanceof Error ? e.message : '加载评论失败'
  } finally {
    loading.value = false
  }
}

onMounted(load)

/**
 * 发表评论。
 *
 * **不做乐观插入**，发完重拉列表。这不是偷懒，是接口形状决定的：
 * `/comment/publish` 只回一句 `{message}`，不给刚创建的那条评论 ——
 * id 和 created_at 都是服务端生成的，前端猜不出来。
 * 唯一能本地拼出来的是 username，而那正好是 comment_handler.go 头顶上
 * 专门解释过一遍的"快照 vs 现值"陷阱：claims 里的名字可能已经过期。
 *
 * 代价是多一次往返（用户会看到自己的评论"顿"一下才出现）。
 * 换来的是列表里的每一行都**确实来自数据库**，而不是一个我们以为对的对象。
 */
async function onPublish() {
  if (publishing.value) return
  const content = draft.value.trim()

  // 这两个本地检查不是装饰，是**绕开后端一个真实的瑕疵**：
  // handler 只查 `content == ""`，全空格的内容会穿透到 service 才被 TrimSpace 拦下，
  // 而 service 返回的是裸 error → 500。前端 trim 之后判空，
  // 就永远不会撞上那条 500 的路径（写 500 的文案去解释"你只打了空格"太荒谬了）
  if (!content) return
  if (!auth.isLoggedIn) {
    notice.value = '登录后才能评论。'
    needLogin.value = true
    return
  }

  publishing.value = true
  notice.value = ''
  needLogin.value = false
  try {
    await publishComment(props.videoId, content)
    draft.value = ''
    await load() // 重拉：新评论在列表最后（后端按 created_at ASC 排）
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      notice.value = '登录状态已失效，请重新登录后再试。'
      needLogin.value = true
    } else if (e instanceof ApiError && e.status === 404) {
      // authorID 查不到账号 → FindByID 返回 gorm.ErrRecordNotFound → 404
      notice.value = '你的账号已经不存在了（可能被注销）。'
      needLogin.value = true
    } else {
      // 草稿留着不清：失败之后让用户重新打一遍是最气人的
      notice.value = e instanceof Error ? e.message : '发布失败'
    }
  } finally {
    publishing.value = false
  }
}

/**
 * 删除评论。
 *
 * **403 和 401 必须分开** —— 这是本项目唯一一处不分开就会出真 bug 的地方：
 * `handleResponse` 对**任何** 401 都调 auth.clearTokens()。
 * 如果"删别人的评论"回的是 401，一个手滑点到别人评论删除键的用户会被静默登出，
 * 而他完全不知道为什么。
 *
 * 所以后端为此专门加了 `apierror.ErrForbidden` → 403（原项目给的是 401）。
 */
async function onDelete(c: CommentItem) {
  if (busyId.value !== null) return
  if (!window.confirm(`确认删除这条评论？\n\n「${c.content.slice(0, 40)}${c.content.length > 40 ? '…' : ''}」`))
    return

  busyId.value = c.id
  notice.value = ''
  needLogin.value = false
  try {
    await deleteComment(c.id)
    // 删除是能本地确定的：我们知道要删的是哪个 id，也知道服务端说成功了。
    // 和发布不一样（那里缺 id 和 created_at），所以这里就地移除，不重拉
    comments.value = comments.value.filter((x) => x.id !== c.id)
  } catch (e) {
    if (e instanceof ApiError && e.status === 403) {
      // 界面上只给自己的评论渲染删除键，所以 403 意味着**列表是旧的**
      // （比如在另一个标签页切过账号）。说清这一点，别让用户以为是"没权限"
      notice.value = '这条评论不是你发的，删不了。列表可能已经过期，刷新一下再看看。'
    } else if (e instanceof ApiError && e.status === 401) {
      notice.value = '登录状态已失效，请重新登录后再试。'
      needLogin.value = true
    } else if (e instanceof ApiError && e.status >= 500) {
      // 500 里混着 "comment not found"（service 返回的裸 error 被兜成 500）。
      // 分不开，那就重拉一次 —— 服务器才是真相
      notice.value = '删除没成功 —— 这条评论可能已经在别处删掉了。'
      await load()
    } else {
      notice.value = e instanceof Error ? e.message : '删除失败'
    }
  } finally {
    busyId.value = null
  }
}
</script>

<style scoped>
.comments {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 18px;
  border-radius: 18px;
}

.comments h2 {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  font-size: 0.95rem;
}
.count {
  color: var(--ink-muted);
  font-size: 0.8rem;
  font-weight: 600;
}
/* 推到最右边：它是这一块的次要操作，不该和标题抢第一眼 */
.reload {
  margin-left: auto;
  min-height: 28px;
  padding: 0 10px;
  border: 1px solid var(--border);
  border-radius: 9px;
  background: transparent;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.74rem;
  cursor: pointer;
}
.reload:hover:not(:disabled) {
  border-color: var(--border-strong);
  color: var(--ink);
}
.reload:disabled {
  cursor: default;
  opacity: 0.6;
}

.cinput {
  width: 100%;
  resize: vertical;
  padding: 11px 13px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.88rem;
  line-height: 1.6;
}
.cinput:focus {
  outline: none;
  border-color: var(--accent-line);
}
.cinput:disabled {
  color: var(--ink-muted);
}

.compose {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.compose .btn {
  min-height: 38px;
  padding: 0 18px;
  font-size: 0.88rem;
}
.hint {
  font-size: 0.72rem;
}

.guestline {
  margin: 0;
  font-size: 0.83rem;
}

.sm {
  margin: 0;
  font-size: 0.83rem;
}

/* ---------- 列表 ---------- */
.list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}

.row {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 12px 0;
  border-top: 1px solid var(--border);
}
/* 第一条不加分隔线：上面刚有一条分隔（发布区/提示），连着两条线很难看 */
.row:first-child {
  border-top: none;
  padding-top: 4px;
}

.cavatar {
  flex: 0 0 auto;
  width: 32px;
  height: 32px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  background: var(--surface-2);
  border: 1px solid var(--accent-line);
  color: var(--accent);
  font-size: 0.82rem;
  font-weight: 700;
}

.body {
  flex: 1 1 auto;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.meta {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  flex-wrap: wrap;
  font-size: 0.76rem;
}
.uname {
  color: var(--accent);
  font-weight: 600;
  text-decoration: none;
}
.uname:hover {
  text-decoration: underline;
}
.uid {
  font-size: 0.7rem;
}

.text {
  margin: 0;
  font-size: 0.87rem;
  line-height: 1.65;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.del {
  flex: 0 0 auto;
  align-self: flex-start;
  min-height: 28px;
  padding: 0 10px;
  border: 1px solid transparent;
  border-radius: 9px;
  background: transparent;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.76rem;
  cursor: pointer;
}
.del:hover:not(:disabled) {
  border-color: var(--border);
  background: var(--surface-2);
  color: var(--danger);
}
.del:disabled {
  cursor: default;
  opacity: 0.6;
}

.soon {
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--surface-2);
  color: var(--ink-muted);
  font-size: 0.68rem;
  font-weight: 600;
}

.note {
  margin: 0;
  font-size: 0.75rem;
  line-height: 1.7;
}

.alert-link {
  margin-left: 6px;
  color: currentColor;
}
</style>
