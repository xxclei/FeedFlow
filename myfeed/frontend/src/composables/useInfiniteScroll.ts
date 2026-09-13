import { onBeforeUnmount, onMounted, type Ref } from 'vue'

/**
 * 滚到底自动翻页。
 *
 * 用 IntersectionObserver 而不是监听 scroll 事件：滚动事件每帧都触发，
 * 要自己节流、自己算 getBoundingClientRect，还会引起强制重排；
 * IntersectionObserver 由浏览器在合成线程上判断，回调频率低得多。
 *
 * rootMargin 提前 400px 触发——等真的滚到底再请求，用户会看到一段空白。
 */
export function useInfiniteScroll(
  sentinel: Ref<HTMLElement | null>,
  canLoad: () => boolean,
  onLoad: () => void,
  rootMargin = '400px 0px',
) {
  let io: IntersectionObserver | null = null

  onMounted(() => {
    if (!sentinel.value) return
    io = new IntersectionObserver(
      (entries) => {
        if (!entries[0]?.isIntersecting) return
        // canLoad 里要挡住"正在加载中"和"没有下一页"，否则会连发请求
        if (canLoad()) onLoad()
      },
      { rootMargin },
    )
    io.observe(sentinel.value)
  })

  onBeforeUnmount(() => io?.disconnect())
}
