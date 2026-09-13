import { computed, onScopeDispose, ref, type ComputedRef, type Ref } from 'vue'

export type Device = 'desktop' | 'mobile'

// 断点 768px：和 Chrome DevTools 的设备模拟、Tailwind 的 md 断点一致，
// 也和项目里已有的 640 / 1024 媒体查询不打架。
const MOBILE_QUERY = '(max-width: 767px)'

/**
 * 端上判定。
 *
 * 用 matchMedia 的**视口宽度**，不用 navigator.userAgent 嗅探。理由：
 *   1. UA 串可以伪造，而且桌面浏览器把窗口拉窄时 UA 一个字都不变——
 *      可 B 站要的恰恰是"窗口窄了就该给移动布局"；
 *   2. matchMedia 自带 change 事件，不用手写 resize 节流；
 *   3. 组件卸载时能精确解绑，不会漏监听。
 *
 * 返回的 device 是响应式的：拖窗口跨过 768px 会立刻翻转，不需要刷新页面。
 */
export function useDevice(): { device: Ref<Device>; isDesktop: ComputedRef<boolean> } {
  const mql = window.matchMedia(MOBILE_QUERY)
  const device = ref<Device>(mql.matches ? 'mobile' : 'desktop')

  const onChange = (e: MediaQueryListEvent) => {
    device.value = e.matches ? 'mobile' : 'desktop'
  }
  mql.addEventListener('change', onChange)

  // 在 setup 作用域里注册清理：App.vue 一旦卸载（HMR 重载也会）自动解绑，
  // 否则热更新几次就会挂上一堆同名监听
  onScopeDispose(() => mql.removeEventListener('change', onChange))

  return {
    device,
    isDesktop: computed(() => device.value === 'desktop'),
  }
}
