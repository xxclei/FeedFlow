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
import { RouterView } from 'vue-router'

import UploadDock from './components/upload/UploadDock.vue'
import DesktopShell from './layouts/DesktopShell.vue'
import MobileShell from './layouts/MobileShell.vue'
import { useDevice } from './composables/useDevice'
import { useUploadQueueStore } from './stores/uploadQueue'

const { isDesktop } = useDevice()
const queue = useUploadQueueStore()
</script>
