<template>
  <!--
    App 只做两件事：
      1. 按屏幕宽度挑一个壳。壳负责导航和版式，业务视图两端共用同一份
         （差异下沉到 components/{desktop,mobile}/ 里的叶子组件和 CSS）。
      2. 挂全局上传浮窗。它在 RouterView 外面，所以换页面不会打断上传。
  -->
  <DesktopShell v-if="isDesktop">
    <RouterView />
  </DesktopShell>
  <MobileShell v-else>
    <RouterView />
  </MobileShell>

  <UploadDock v-if="queue.hasActivity" />
</template>

<script setup lang="ts">
import { watch } from 'vue'
import { RouterView } from 'vue-router'

import UploadDock from './components/upload/UploadDock.vue'
import DesktopShell from './layouts/DesktopShell.vue'
import MobileShell from './layouts/MobileShell.vue'
import { useDevice } from './composables/useDevice'
import { useAuthStore } from './stores/auth'
import { useNotificationStore } from './stores/notification'
import { useUploadQueueStore } from './stores/uploadQueue'

const { isDesktop } = useDevice()
const queue = useUploadQueueStore()
const auth = useAuthStore()
const notifications = useNotificationStore()

/**
 * 通知的 SSE 连接**挂在这一层**，不挂在铃铛组件里。
 *
 * 两个理由，第二个是硬的：
 *
 *	① App 是"登录态之上的那一层"：它在整个会话里只挂载一次，
 *	   而通知连接的生死**本来就由登录态决定**（登出必须关掉，见 store）。
 *	   把触发点和它依赖的状态放在同一个地方，是这里最自然的结构。
 *
 *	② **壳会随断点切换而重新挂载。** 铃铛在 DesktopShell / MobileShell 里，
 *	   拖动窗口跨过 768px 就会换一个壳 —— 连接要是挂在铃铛的 onMounted 里，
 *	   每拖一次窗口就断开重连一次，而"断开到重连"中间那几秒收到的推送
 *	   正好落在没人订阅的窗口里（后端 SSEHub 的注册表里那时没有这条连接，
 *	   按设计**直接丢掉不补**）。换成 App 这一层，壳怎么换都不影响。
 *
 * immediate: true 不能省：带着 token 刷新页面时，isLoggedIn 一上来就是 true，
 * 不立刻求值的话 watch 要等到**下一次登录态变化**才被触发 —— 也就是永远不连。
 */
watch(
  () => auth.isLoggedIn,
  (loggedIn) => {
    if (loggedIn) notifications.connect()
    else notifications.disconnect()
  },
  { immediate: true },
)
</script>
