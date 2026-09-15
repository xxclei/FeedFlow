<template>
  <div class="page">
    <!-- 移动端壳的顶栏没有返回键，被"推进去"的一层得自己给出口（同详情页/点赞页）。
         但这一页的返回是**两段式**的：聊天窗（/messages/5）返回的是联系人列表
         （/messages），联系人列表返回的才是上一页 -->
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
      {{ peerID ? '联系人' : '返回' }}
    </button>

    <div class="head">
      <div class="head-left">
        <h1>私信</h1>
        <p class="lead mono">POST /message/send · /message/list · 无会话表，无游标，LIMIT 50</p>
      </div>
      <!-- **这个按钮是刻意的，不是没做完。**

           后端没有推送私信的能力（/notification/stream 只推通知，而且私信
           压根不产生通知），前端也不打算在这里自己起一个轮询：私信是
           "低频、可等待"的场景，为它每 3 秒打一次 /message/list，
           换来的只是"对方回得够快的话你能早几秒看见"——
           代价是每个开着聊天窗的人都在持续产生查询。

           要实时化，正确做法是**复用阶段9那条 SSE**：把 message 事件也推一份
           到 /notification/stream 上（或者单开一条 /message/stream），
           前端在这条连接上再挂一个监听。那是进阶改进，不是这里的 TODO。 -->
      <button
        class="btn btn-ghost sm"
        type="button"
        :disabled="chatLoading || !peerID"
        @click="loadMessages"
      >
        刷新
      </button>
    </div>

    <!-- 非法 peerId：本地就判掉，一个请求都不发（同 ProfileView/VideoDetailView）。
         注意它和"合法但不存在的账号"**不是**一回事 —— 后者后端不校验，
         会正常返回一个空会话 -->
    <div v-if="peerInvalid" class="empty-block">
      <p class="empty-title">地址里的账号 ID 不合法</p>
      <p class="text-muted">
        <code>{{ rawPeerID }}</code> 不是正整数。私信地址是
        <code>/messages/对方的账号ID</code>，比如 <code>/messages/3</code>。
      </p>
      <RouterLink class="btn" to="/messages">看联系人列表</RouterLink>
    </div>

    <div v-else class="cols">
      <!-- ---------- 左：联系人 ---------- -->
      <aside v-if="showContacts" class="card col-list">
        <h2>联系人</h2>
        <!-- 联系人列表是**前端拼的**，后端没有这个接口（原项目也没有）：
             我关注的人 ∪ 我的粉丝，去掉自己。理由写在 message/entity.go 末尾 ——
             产品要的是"能聊天的人"（= 关注关系），而不是"聊过天的人" -->
        <p class="hint text-muted">
          我关注的人 ∪ 我的粉丝。顺序是**接口返回的顺序**，不是"最近聊过" ——
          那需要为每个人查一次最后一条消息。
        </p>

        <p v-if="contactsLoading" class="hint text-muted">读取中…</p>
        <p v-else-if="contactsError" class="alert">{{ contactsError }}</p>

        <ul v-else-if="contacts.length" class="contacts">
          <li v-for="c in contacts" :key="c.id">
            <RouterLink class="contact" :class="{ on: c.id === peerID }" :to="`/messages/${c.id}`">
              <span class="avatar" aria-hidden="true">{{ c.username.slice(0, 1).toUpperCase() }}</span>
              <span class="who">
                <span class="name">{{ c.username }}</span>
                <span class="mono uid">UID {{ c.id }}</span>
              </span>
            </RouterLink>
          </li>
        </ul>

        <div v-else class="empty-block">
          <p class="empty-title">还没有能私信的人</p>
          <p class="text-muted">
            关注一个人、或者被一个人关注之后，TA 就会出现在这里。
          </p>
          <RouterLink class="btn" to="/feed">去发现页</RouterLink>
        </div>
      </aside>

      <!-- ---------- 右：聊天窗 ---------- -->
      <section v-if="showChat" class="card col-chat">
        <template v-if="peerID">
          <header class="chat-head">
            <span class="avatar" aria-hidden="true">{{ peerName.slice(0, 1).toUpperCase() || '?' }}</span>
            <div class="chat-who">
              <strong>{{ peerName || `UID ${peerID}` }}</strong>
              <span class="mono uid">UID {{ peerID }}</span>
            </div>
          </header>

          <div ref="scroller" class="messages">
            <p v-if="chatLoading" class="hint text-muted">读取中…</p>
            <p v-else-if="chatError" class="alert">{{ chatError }}</p>

            <!-- **空会话的空态**：后端对空会话返回的是 `{"messages":null}`
                 （Go 里的 nil slice），api/message.ts 已经兜成 `[]` 了 ——
                 不兜的话这里 `.length` 直接 TypeError，整页白屏 -->
            <div v-else-if="!messages.length" class="empty-chat text-muted">
              <p>还没有聊过。说点什么吧 —— 第一句话总是最难的那句。</p>
            </div>

            <template v-else>
              <div
                v-for="m in messages"
                :key="m.id"
                class="bubble-row"
                :class="{ mine: m.from_id === myID }"
              >
                <div class="bubble">
                  <p class="text">{{ m.content }}</p>
                  <time class="mono when" :title="fullTime(isoToUnixSeconds(m.created_at))">
                    {{ formatTime(isoToUnixSeconds(m.created_at)) }}
                  </time>
                </div>
              </div>
            </template>
          </div>

          <!-- 发不出去时把原因摆在输入框上方，而不是弹个 alert：
               用户刚打完字，视线就在这儿 -->
          <p v-if="sendError" class="alert send-err">{{ sendError }}</p>

          <form class="composer" @submit.prevent="onSend">
            <input
              v-model="draft"
              type="text"
              maxlength="1000"
              placeholder="说点什么…"
              :disabled="sending"
            />
            <button class="btn" type="submit" :disabled="sending || !draft.trim()">
              {{ sending ? '发送中…' : '发送' }}
            </button>
          </form>
        </template>

        <!-- /messages（桌面端右侧、还没选人） -->
        <div v-else class="empty-chat text-muted">
          <p>从左边选一个人开始聊天。</p>
        </div>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import type { AccountInfo } from '../api/account'
import { findByID } from '../api/account'
import { ApiError } from '../api/client'
import { listMessages, sendMessage, type Message } from '../api/message'
import { getAllFollowers, getAllVloggers } from '../api/social'
import { useDevice } from '../composables/useDevice'
import { useAuthStore } from '../stores/auth'
import { formatTime, fullTime, isoToUnixSeconds } from '../utils/time'

/**
 * 私信页（阶段12）。一条路由，两个身份：
 *
 *	/messages         → 联系人列表（桌面端右边是空的"选一个人"）
 *	/messages/:peerId → 同一个组件，右边是聊天窗
 *
 * 为什么合成一个组件而不是两个页面：两栏布局在桌面端**同时可见**，
 * 拆成两个视图就得把联系人列表复制一份，或者让它被两个路由共享 ——
 * 前者必然分叉，后者要把"选中了谁"提到组件外面。这里最自然的表达
 * 就是"同一屏，右边随 peerId 变"。
 *
 * ---------- 数据是三个接口拼的 ----------
 *
 *	getAllVloggers / getAllFollowers → 联系人（我关注的 ∪ 我的粉丝，去掉自己）
 *	listMessages                     → 会话历史
 *	sendMessage                      → 发一条
 *
 * 没有"联系人列表"接口，也没有"会话列表"接口 —— 后者更值得说一句：
 * 后端只有一张 messages 表，会话是 `(from,to) OR (to,from)` 推算的，
 * 所以"我有哪些会话"这个查询**存在但没被实现**（要 GROUP BY 对方 id，
 * 而原项目没做）。前端因此连"最后一条消息预览"都给不出来。
 */
const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const { isDesktop } = useDevice()

const myID = computed(() => auth.claims?.account_id ?? 0)

// ---------- 路由参数 ----------
const rawPeerID = computed(() => String(route.params.peerId ?? ''))
const peerInvalid = ref(false)
/** 当前聊天对象。**0 = 没选人**（/messages 那条路由） */
const peerID = ref(0)

// 移动端一次只显示一栏：有 peerId 就是聊天窗，没有就是联系人列表。
// 桌面端两栏都显示（右栏在没选人时是一句提示）
const showContacts = computed(() => isDesktop.value || !peerID.value)
const showChat = computed(() => isDesktop.value || !!peerID.value)

// ---------- 联系人 ----------
const contacts = ref<AccountInfo[]>([])
const contactsLoading = ref(false)
const contactsError = ref('')

// ---------- 会话 ----------
const messages = ref<Message[]>([])
const chatLoading = ref(false)
const chatError = ref('')
const peerName = ref('')

const draft = ref('')
const sending = ref(false)
const sendError = ref('')
const scroller = ref<HTMLElement | null>(null)

/**
 * 联系人 = 我关注的人 ∪ 我的粉丝。
 *
 * 两个接口**并行发**（不是串行）：它们之间没有任何依赖，串起来只是把
 * 两次往返叠成一次的长。
 *
 * 去重是**必须的**：互相关注的人会同时出现在两个列表里（这是最常见的情况，
 * 不是边界情况），不去重的话联系人列表里同一个人出现两次。
 * 用 Map 而不是 filter+some：后者是 O(n²)，500 个关注就是 25 万次比较。
 *
 * 去掉自己：理论上自己不会出现在这两个列表里（后端拦了自关注），
 * 但**前端不该依赖后端替它保证展示层的事** —— 多一行判断，换掉一种
 * "点进自己的私信和自己聊天"的荒唐画面。
 */
async function loadContacts() {
  contactsLoading.value = true
  contactsError.value = ''
  try {
    const [vloggers, followers] = await Promise.all([getAllVloggers(), getAllFollowers()])
    const merged = new Map<number, AccountInfo>()
    for (const a of vloggers?.vloggers ?? []) merged.set(a.id, a)
    for (const a of followers?.followers ?? []) {
      // set 会覆盖同 id 的旧值 —— 两个接口返回的是同一个账号的同一份数据，
      // 覆盖与否都一样；写成"已存在就不动"只是少一次无意义的赋值
      if (!merged.has(a.id)) merged.set(a.id, a)
    }
    merged.delete(myID.value)
    contacts.value = [...merged.values()]
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      contactsError.value = '登录状态已失效，请重新登录后再试。'
    } else {
      contactsError.value = e instanceof Error ? e.message : '加载联系人失败'
    }
  } finally {
    contactsLoading.value = false
  }
}

/**
 * 拉会话历史。**手动刷新按钮调的就是它**（见模板里那段说明：刻意不做轮询）。
 */
async function loadMessages() {
  const id = peerID.value
  if (!id) {
    messages.value = []
    return
  }
  chatLoading.value = true
  chatError.value = ''
  try {
    const raw = await listMessages(id)
    // 快速切换联系人时上一轮可能后到 —— 认领结果前先确认还停在同一个人身上
    // （同 ProfileView.loadVideos 那条守卫）
    if (peerID.value !== id) return
    // **必须翻转。** 后端是 `Order("created_at desc")`（最新的在前，它把这
    // 当"最近消息"列表用），而聊天窗要的是正序：老的在上、新的在下。
    // slice() 先复制再 reverse —— 直接 reverse 会就地把 raw 改了，
    // 虽然这里是局部变量无所谓，但保持"不改输入"的习惯省得哪天抄到别处出事
    messages.value = raw.slice().reverse()
    scrollToBottom()
  } catch (e) {
    if (peerID.value !== id) return
    chatError.value = e instanceof Error ? e.message : '加载会话失败'
  } finally {
    chatLoading.value = false
  }
}

/**
 * 聊天对象的显示名。
 *
 * 优先从联系人列表里找（**不需要额外请求**）。找不到很正常：从别人主页点
 * "私信"进来时，那个人可能既没关注我、我也没关注他 —— 他不在联系人里。
 * 这时退化成一次 /account/findByID（公开接口）把名字补上。
 *
 * 只补名字、**不把他塞进联系人列表**：联系人列表的语义是"能私信的人"
 * （= 关注关系），塞进去会让它慢慢变成"聊过天的人"，两个语义就混了。
 */
async function loadPeerName() {
  const id = peerID.value
  if (!id) {
    peerName.value = ''
    return
  }
  const hit = contacts.value.find((c) => c.id === id)
  if (hit) {
    peerName.value = hit.username
    return
  }
  peerName.value = ''
  try {
    const acc = await findByID(id)
    if (peerID.value !== id) return
    peerName.value = acc?.username ?? ''
  } catch {
    // 查不到就不显示名字，退化成"UID 5"（模板里已经兜了）——
    // 一个只影响标题的请求失败，不值得让整页报错
  }
}

/**
 * 发送。
 *
 * **成功后不重新拉列表**：`/message/send` 返回的就是落库后的那一行
 * （含 id 和 created_at），直接追加到数组尾部即可。少一次往返，
 * 也少一次"发完列表闪一下"。
 *
 * 但要注意**追加而不是等推送**：后端没有"新私信"的推送通道，
 * 所以这条消息在对方那边只能靠他点刷新才会出现 —— 我们这边立刻能看到，
 * 是因为这个对象是我们自己发出去的，不是从服务器推来的。
 */
async function onSend() {
  const text = draft.value.trim()
  const id = peerID.value
  if (!text || !id || sending.value) return

  sending.value = true
  sendError.value = ''
  try {
    const saved = await sendMessage(id, text)
    if (peerID.value !== id) return
    messages.value.push(saved)
    draft.value = ''
    scrollToBottom()
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      sendError.value = '登录状态已失效，请重新登录后再试。'
    } else if (e instanceof ApiError && e.status === 400) {
      // 后端对空内容/缺 to_id 返 400。前端已经 trim 过了，走到这儿
      // 通常是把超过 text 列容量的内容发出去了 —— 原样显示后端的话更准
      sendError.value = e.message || '这条消息发不出去（内容为空？）'
    } else {
      sendError.value = e instanceof Error ? e.message : '发送失败'
      // **不清空输入框**：发失败了还把用户打的字清掉，是最气人的那种交互
    }
  } finally {
    sending.value = false
  }
}

/** 滚到底。必须在 nextTick 之后 —— DOM 还没渲染出来时 scrollHeight 是旧值 */
function scrollToBottom() {
  void nextTick(() => {
    const el = scroller.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

/**
 * 从路由参数推导"当前是谁"，并把这一轮的数据全拉一遍。
 *
 * 路由参数是**字符串**：`/messages/abc` 直接透给后端，`PeerID uint`
 * 反序列化失败 → ShouldBindJSON 报错 → 400，页面上挂一条 Go 的英文报错。
 * 所以先在本地转数字（同 ProfileView / VideoDetailView）。
 */
async function syncFromRoute() {
  const raw = rawPeerID.value
  if (!raw) {
    // /messages：联系人列表，右边空着
    peerID.value = 0
    peerInvalid.value = false
    messages.value = []
    peerName.value = ''
    chatError.value = ''
    return
  }

  const id = Number(raw)
  if (!Number.isInteger(id) || id <= 0) {
    peerInvalid.value = true
    peerID.value = 0
    return
  }
  peerInvalid.value = false
  peerID.value = id
  draft.value = ''
  sendError.value = ''

  await loadPeerName()
  await loadMessages()
}

onMounted(async () => {
  document.title = '私信 · myfeed'
  await loadContacts()
  await syncFromRoute()
})

// 联系人之间来回点（/messages/1 → /messages/2）视图是**复用**的，
// 必须重新拉会话 + 滚回顶部，否则会出现"地址变了、消息还是上一个人的"。
// 和 ProfileView / VideoDetailView 对 route.params 的 watch 同一套
watch(
  () => route.params.peerId,
  () => {
    window.scrollTo(0, 0)
    void syncFromRoute()
  },
)

// 标题还原用"进来之前是什么"，不是硬编码回 'myfeed' ——
// 别的视图（详情页/主页）也会写 document.title，硬还原会把它们的标题也冲掉
const prevTitle = document.title
onUnmounted(() => {
  document.title = prevTitle
})

function goBack() {
  // 聊天窗返回联系人列表：**这是这一页内部的一跳**，不走 history.back()
  // （从 /messages 直接进 /messages/5 的话，back 会退到上一页而不是列表）
  if (peerID.value) {
    router.push('/messages')
    return
  }
  if (window.history.state?.back) router.back()
  else router.replace('/feed')
}
</script>

<style scoped>
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
  gap: 14px;
  margin-bottom: 18px;
}
h1 {
  margin: 0 0 4px;
  font-size: 1.3rem;
  letter-spacing: -0.02em;
}
.lead {
  margin: 0;
  font-size: 0.72rem;
  color: var(--ink-muted);
  overflow-wrap: anywhere;
}

/* 两栏：左联系人定宽，右会话吃掉剩下全部。
   1fr 而不是 auto —— 会话里最长的一行消息决定不了栏宽，
   否则一条长消息会把布局撑破 */
.cols {
  display: grid;
  grid-template-columns: 260px 1fr;
  gap: 16px;
  align-items: start; /* 不加的话两栏会等高，短的那栏被拉出一片空白 */
}

.col-list {
  padding: 16px;
  border-radius: 18px;
}
.col-list h2 {
  margin: 0 0 6px;
  font-size: 1rem;
  letter-spacing: -0.02em;
}
.hint {
  margin: 0 0 12px;
  font-size: 0.76rem;
  line-height: 1.6;
}

.contacts {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 60vh;
  overflow-y: auto;
}
.contact {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border-radius: 12px;
  transition: background 0.18s var(--ease-out);
}
.contact:hover {
  background: var(--surface-2);
}
.contact.on {
  background: var(--accent-soft);
  box-shadow: inset 0 0 0 1px var(--accent-line);
}
.who {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.name {
  font-size: 0.88rem;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.uid {
  color: var(--ink-muted);
  font-size: 0.68rem;
}

.avatar {
  flex: 0 0 auto;
  width: 32px;
  height: 32px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  background: var(--surface-2);
  border: 1px solid var(--accent-line);
  color: var(--accent);
  font-weight: 700;
  font-size: 0.82rem;
}

/* ---------- 聊天窗 ---------- */
.col-chat {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
  border-radius: 18px;
  /* 定高而不是"内容撑开"：输入框要**钉在底部**，而消息区自己滚。
     用内容撑高的话，聊到第 50 条时整页变成几屏长，输入框跑到屏幕外 ——
     用户每发一条都得先滚到底 */
  height: min(64vh, 560px);
}

.chat-head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--border);
}
.chat-who {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.chat-who strong {
  font-size: 0.95rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.messages {
  flex: 1 1 auto;
  min-height: 0; /* flex 子项默认 min-height:auto，不写这条 overflow 不生效 */
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-right: 4px;
}

.bubble-row {
  display: flex;
  justify-content: flex-start;
}
.bubble-row.mine {
  justify-content: flex-end;
}
.bubble {
  max-width: 76%;
  padding: 8px 12px;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface-2);
}
/* 自己发的：主色柔光底 + 右下角切一个小角，方向感靠这一下就出来了
   （不用头像 —— 会话是两个人，头像只会占宽度） */
.bubble-row.mine .bubble {
  background: var(--accent-soft);
  border-color: var(--accent-line);
  border-bottom-right-radius: 4px;
}
.text {
  margin: 0;
  font-size: 0.88rem;
  line-height: 1.6;
  /* 长串英文/URL 不换行会把气泡撑破，整个聊天窗跟着横向滚 */
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.when {
  display: block;
  margin-top: 2px;
  color: var(--ink-muted);
  font-size: 0.66rem;
  text-align: right;
}

.empty-chat {
  margin: auto; /* 消息区是 flex 容器，auto 外边距把空态推到正中央 */
  text-align: center;
  font-size: 0.84rem;
}

.composer {
  display: flex;
  gap: 8px;
}
.composer input {
  flex: 1;
  min-width: 0;
  height: 42px;
  padding: 0 14px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.9rem;
  outline: none;
}
.composer input:focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}
.composer .btn {
  flex: 0 0 auto;
  min-height: 42px;
  padding: 0 18px;
  font-size: 0.9rem;
}
.send-err {
  margin: 0;
}

/* 移动端：一栏。`1fr` 覆盖掉上面那条 260px 1fr —— 不改的话
   窄屏会被 260px 的定宽列挤成横向滚动 */
@media (max-width: 767px) {
  .cols {
    grid-template-columns: 1fr;
  }
  .col-chat {
    /* 手机上 dvh 比 vh 准（地址栏收起/展开时 vh 不变，底部会被压住） */
    height: min(70dvh, 520px);
  }
  h1 {
    font-size: 1.1rem;
  }
  .lead {
    display: none;
  }
  .head {
    align-items: stretch;
    flex-direction: column;
    gap: 10px;
  }
}
</style>
