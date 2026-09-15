<template>
  <!-- 未登录整个不渲染：/notification/* 四条全挂鉴权，游客点开必然 401，
       而 client.ts 对任何 401 都 clearTokens —— 摆出来只会让游客莫名被登出。
       比"灰掉"更彻底是对的：铃铛和「关注流」那个灰态频道不同，
       那个是在告诉他"有这么个东西，登录了才有"，而通知中心对游客
       连一句话都说不出来（他连"自己的通知"都不存在） -->
  <div v-if="auth.isLoggedIn" ref="wrapper" class="bell-wrap">
    <button
      class="bell"
      type="button"
      :aria-expanded="open"
      aria-label="通知"
      :title="title"
      @click="toggle"
    >
      <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">
        <path
          d="M6.5 9.8a5.5 5.5 0 0 1 11 0c0 4 1.5 5.4 1.5 5.4H5s1.5-1.4 1.5-5.4M10 18.4a2.2 2.2 0 0 0 4 0"
          fill="none"
          stroke="currentColor"
          stroke-width="1.9"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
      <!-- 红点数字。>99 显示 99+：三位数会把铃铛撑变形，而这个数字
           本来就只是"有东西"的提示，不是精确计数 -->
      <span v-if="badge" class="badge" aria-hidden="true">{{ badge }}</span>
    </button>

    <div v-if="open" class="panel">
      <header class="panel-head">
        <strong>通知</strong>
        <button
          class="mark-all"
          type="button"
          :disabled="!store.unread"
          @click="onMarkAll"
        >
          全部已读
        </button>
      </header>

      <!-- SSE 断了才提示。**不能默认显示**：这条连接绝大多数时候是活的，
           常驻一句"实时推送已连接"只是噪音（同"系统运行正常"那种日志） -->
      <p v-if="!store.connected" class="panel-note text-muted">
        实时推送未连上 —— 现在看到的可能不是最新的。
      </p>

      <p v-if="store.loading" class="panel-note text-muted">读取中…</p>
      <p v-else-if="store.error" class="panel-note err">{{ store.error }}</p>

      <ul v-else-if="store.items.length" class="list">
        <li v-for="n in store.items" :key="n.id">
          <button class="item" :class="{ on: !n.is_read }" type="button" @click="onOpen(n)">
            <span class="pip" aria-hidden="true"></span>
            <span class="body">
              <!-- 后端只给 sender_id，**不给发件人昵称**，这里就用 UID 顶上。
                   想显示昵称只有一条路：每条通知调一次 /account/findByID ——
                   那是 50 条通知 50 个请求（N+1），为了几个字不值得。
                   真正的修法是在通知表里冗余一列 sender_name，或者加个批量接口。
                   顺带一提，**content 是后端渲染好的整句话**（"点赞了你的视频"），
                   所以这里不拼"xxx 赞了你的视频"，直接把 content 贴出来 -->
              <span class="line">
                <span class="mono uid">UID {{ n.sender_id }}</span>
                <span>{{ n.content }}</span>
              </span>
              <time class="mono when" :title="fullTime(isoToUnixSeconds(n.created_at))">
                {{ formatTime(isoToUnixSeconds(n.created_at)) }}
              </time>
            </span>
          </button>
        </li>
      </ul>

      <p v-else class="panel-note text-muted">
        还没有通知。别人点赞、评论你的视频或者关注你时，会出现在这里。
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'

import { notificationTarget, type NotificationItem } from '../api/notification'
import { useAuthStore } from '../stores/auth'
import { useNotificationStore } from '../stores/notification'
import { formatTime, fullTime, isoToUnixSeconds } from '../utils/time'

/**
 * 通知铃铛 + 下拉面板。桌面壳和移动壳**共用同一个组件** ——
 * 差异只有外面那圈的间距，值不得拆两份（拆了就是两份要同步维护的
 * "点开拉历史/点了跳路由"逻辑）。
 *
 * ---------- 面板里的连接不归它管 ----------
 *
 * 它只负责"显示 + 触发 load()"，connect/disconnect 由 App.vue 按登录态来做。
 * 理由写在 stores/notification.ts 顶上：壳会随断点切换而重新挂载，
 * 连接挂在组件里就会跟着断一次。
 */
const auth = useAuthStore()
const store = useNotificationStore()
const router = useRouter()

const open = ref(false)
const wrapper = ref<HTMLElement | null>(null)

const badge = computed(() => {
  if (!store.unread) return ''
  return store.unread > 99 ? '99+' : String(store.unread)
})

const title = computed(() =>
  store.unread ? `${store.unread} 条未读通知` : '通知',
)

async function toggle() {
  open.value = !open.value
  if (open.value) {
    // 打开时才拉历史。**每次打开都拉**，不做"拉过就不拉"的缓存 ——
    // 推送会丢（后端 Push 是缓冲满就丢的非阻塞发送），表才是真相源，
    // 而这次查询只是后端的一次 LIMIT 50
    await store.load()
  }
}

function onOpen(n: NotificationItem) {
  open.value = false
  // 标已读和跳转**不互相等待**：跳转是用户真正要的结果，
  // 而"标已读"失败只影响红点，下次 load() 会以服务端为准修回来
  void store.markOne(n.id)
  void router.push(notificationTarget(n))
}

function onMarkAll() {
  void store.markAllRead()
}

/**
 * 点外面关掉。
 *
 * 用 wrapper.contains 判断而不是"给面板加 @click.stop"：
 * 后者遇到面板里将来新增的、把点击事件重新派发出去的控件就会漏。
 * 顺带用 Esc 也能关 —— 下拉面板不给键盘出口是很常见的小毛病。
 *
 * 监听只在打开时挂（而不是常驻 + 判断 open）：一个常驻的 document 监听
 * 在每次点击时都会跑一遍，而它 99% 的时间什么也不做。
 */
function onDocPointerDown(e: MouseEvent) {
  if (!wrapper.value?.contains(e.target as Node)) open.value = false
}
function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') open.value = false
}

watch(open, (v) => {
  if (v) {
    document.addEventListener('mousedown', onDocPointerDown)
    document.addEventListener('keydown', onKeydown)
  } else {
    document.removeEventListener('mousedown', onDocPointerDown)
    document.removeEventListener('keydown', onKeydown)
  }
})

// 组件卸载（拖窗口跨断点换了壳）时**必须摘掉这两个监听**：
// 挂着的话，旧壳留下的监听会一直往一个已经卸载的组件里写 open.value，
// 而且每换一次壳就多一个。这里不需要额外判断 —— watch 的清理写在上面，
// 但 watch 只在下一次 open 变化时才跑，卸载时不会触发，所以这里再兜一次。
onUnmounted(() => {
  document.removeEventListener('mousedown', onDocPointerDown)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<style scoped>
.bell-wrap {
  position: relative; /* 面板以它为锚点右对齐，不写的话会飞到 <body> 上去 */
  flex: 0 0 auto;
}

.bell {
  position: relative;
  display: grid;
  place-items: center;
  width: 38px;
  height: 38px;
  border: none;
  border-radius: 12px;
  background: transparent;
  color: var(--ink);
  cursor: pointer;
  transition: background 0.18s var(--ease-out);
}
.bell:hover {
  background: var(--surface-2);
}

.badge {
  position: absolute;
  top: 2px;
  right: 2px;
  min-width: 17px;
  height: 17px;
  padding: 0 4px;
  border-radius: 999px;
  background: var(--danger);
  color: #fff;
  font-family: var(--font-mono);
  font-size: 0.64rem;
  font-weight: 700;
  line-height: 17px;
  text-align: center;
  /* 数字是白字压在红底上、红底又压在深色顶栏上，边界靠这一圈描边拉开 */
  border: 1.5px solid var(--canvas);
}

.panel {
  position: absolute;
  top: calc(100% + 10px);
  right: 0;
  z-index: 50; /* 顶栏是 z-index:30，面板必须压过它 */
  width: 340px;
  /* 移动端窄屏：铃铛贴着右边缘，340px 会捅出屏幕。
     用 max-width 而不是媒体查询 —— 断点在这里是多余的复杂度 */
  max-width: calc(100vw - 24px);
  border: 1px solid var(--border);
  border-radius: 16px;
  background: var(--surface);
  box-shadow: var(--shadow);
  overflow: hidden;
}

.panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 14px;
  border-bottom: 1px solid var(--border);
  font-size: 0.9rem;
}
.mark-all {
  border: none;
  background: transparent;
  color: var(--accent);
  font-family: inherit;
  font-size: 0.8rem;
  font-weight: 600;
  cursor: pointer;
}
.mark-all:disabled {
  color: var(--ink-muted);
  cursor: not-allowed;
}

.panel-note {
  margin: 0;
  padding: 14px;
  font-size: 0.82rem;
  line-height: 1.6;
}
.panel-note.err {
  color: var(--danger);
}

.list {
  list-style: none;
  margin: 0;
  padding: 0;
  /* 不给固定高度，用 max-height：通知少的时候面板就是短的，
     不会留一大片空 —— 这和"列表页永远撑满"是两种场景 */
  max-height: 380px;
  overflow-y: auto;
}
.list li + li {
  border-top: 1px solid var(--border);
}

.item {
  display: flex;
  align-items: flex-start;
  gap: 9px;
  width: 100%;
  padding: 11px 14px;
  border: none;
  background: transparent;
  color: var(--ink);
  font-family: inherit;
  font-size: 0.84rem;
  text-align: left;
  cursor: pointer;
  transition: background 0.18s var(--ease-out);
}
.item:hover {
  background: var(--surface-2);
}
/* 未读：左侧一个小圆点，不用整行变色 —— 整行染色在深色主题下
   会把"未读"和"悬停"两种状态混成一样 */
.pip {
  flex: 0 0 auto;
  width: 7px;
  height: 7px;
  margin-top: 6px;
  border-radius: 50%;
  background: transparent;
}
.item.on .pip {
  background: var(--accent);
}

.body {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}
.line {
  display: flex;
  flex-wrap: wrap;
  gap: 5px;
  overflow-wrap: anywhere;
}
.uid {
  color: var(--ink-muted);
}
.when {
  color: var(--ink-muted);
  font-size: 0.7rem;
}
</style>
