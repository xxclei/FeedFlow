import { computed, reactive, ref } from 'vue'

import { listByPopularity, listLatest, listLikesCount, type FeedVideoItem } from '../api/feed'

export type TabKey = 'latest' | 'likes' | 'popularity'

export const FEED_TABS: { key: TabKey; label: string; hint: string }[] = [
  { key: 'latest', label: '最新', hint: 'POST /feed/listLatest · 单值游标 latest_time（毫秒）' },
  { key: 'likes', label: '点赞榜', hint: 'POST /feed/listLikesCount · 复合游标 (likes_count, id)' },
  {
    key: 'popularity',
    label: '热门榜',
    hint: 'POST /feed/listByPopularity · 三键游标 (popularity, create_time, id)',
  },
]

export function isTabKey(v: unknown): v is TabKey {
  return v === 'latest' || v === 'likes' || v === 'popularity'
}

export interface StreamState {
  items: FeedVideoItem[]
  hasMore: boolean
  loading: boolean
  loaded: boolean
  error: string
  dupCount: number // 跨页重复计数：游标失效的探针
  // 每个流自己的游标，服务端返回什么就原样存什么
  cTime: number
  cLikes?: number
  cID?: number
  cPopularity?: number
  cBefore?: string
  cLatestID?: number
}

function newState(): StreamState {
  return {
    items: [],
    hasMore: false,
    loading: false,
    loaded: false,
    error: '',
    dupCount: 0,
    cTime: 0,
  }
}

/**
 * 三个 Feed 流的游标状态机。
 *
 * 抽成 composable 的动机：桌面壳和移动壳要的是**同一份数据逻辑**，只有渲染不同。
 * 如果把它写在 FeedView 里，两个端要么复制粘贴一份，要么被迫把视图也写两份。
 *
 * **状态挂在模块作用域，不是每次调用新建。**
 * 因为 App.vue 里没有 `<KeepAlive>`：点卡片进播放页会把 FeedView 卸载，
 * 再回来是重新 setup。状态建在函数里的话，三个流的游标、已加载的每一页、
 * DebugPanel 的跨页重复探针会全部归零 —— 用户按一次返回就得重新从第一页刷。
 * 提上来之后数据活着，配合路由的 scrollBehavior 还能把滚动位置也还原回去。
 *
 * 代价是三个流常驻内存（每流几百条 item 封顶，翻页游标只增不减）。这个交换很划算。
 * 单例由**第一个**调用方决定 limit；目前只有 FeedView 一个调用方。
 */
let singleton: ReturnType<typeof createStreams> | null = null

export function useFeedStream(limit = 12) {
  if (!singleton) singleton = createStreams(limit)
  return singleton
}

function createStreams(limit: number) {
  const states = reactive<Record<TabKey, StreamState>>({
    latest: newState(),
    likes: newState(),
    popularity: newState(),
  })

  const active = ref<TabKey>('latest')
  const cur = computed(() => states[active.value])

  async function load(key: TabKey, reset: boolean) {
    const st = states[key]
    if (st.loading) return
    st.loading = true
    st.error = ''

    try {
      let list: FeedVideoItem[] = []

      if (key === 'latest') {
        const res = await listLatest({ limit, latest_time: reset ? 0 : st.cTime })
        list = res.video_list
        st.hasMore = res.has_more
        st.cTime = res.next_time
      } else if (key === 'likes') {
        // reset 时两个游标都不传（undefined）→ 后端当作首页
        const res = await listLikesCount({
          limit,
          likes_count_before: reset ? undefined : st.cLikes,
          id_before: reset ? undefined : st.cID,
        })
        list = res.video_list
        st.hasMore = res.has_more
        st.cLikes = res.next_likes_count_before
        st.cID = res.next_id_before
      } else {
        const res = await listByPopularity({
          limit,
          as_of: 0,
          offset: 0,
          latest_popularity: reset ? undefined : st.cPopularity,
          latest_before: reset ? undefined : st.cBefore,
          latest_id_before: reset ? undefined : st.cLatestID,
        })
        list = res.video_list
        st.hasMore = res.has_more
        st.cPopularity = res.next_latest_popularity
        st.cBefore = res.next_latest_before
        st.cLatestID = res.next_latest_id_before
      }

      if (reset) {
        st.items = list
        st.dupCount = 0
      } else {
        // 探针：游标一旦失效，同一条视频会在相邻两页里重复出现
        const seen = new Set(st.items.map((x) => x.id))
        st.dupCount += list.filter((x) => seen.has(x.id)).length
        st.items = st.items.concat(list)
      }
      st.loaded = true
    } catch (e) {
      st.error = e instanceof Error ? e.message : String(e)
    } finally {
      st.loading = false
    }
  }

  /** 首次切到某个流时才去拉数据（懒加载），重复切回不重拉 */
  function ensure(key: TabKey) {
    if (!states[key].loaded && !states[key].loading) return load(key, true)
    return Promise.resolve()
  }

  function setTab(key: TabKey) {
    active.value = key
    return ensure(key)
  }

  return { states, active, cur, load, ensure, setTab, limit }
}
