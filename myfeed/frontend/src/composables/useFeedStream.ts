import { computed, reactive, ref } from 'vue'

import {
  listByFollowing,
  listByPopularity,
  listLatest,
  listLikesCount,
  type FeedVideoItem,
} from '../api/feed'

export type TabKey = 'latest' | 'likes' | 'popularity' | 'following'

export interface TabDef {
  key: TabKey
  label: string
  hint: string
  /** 这个流必须登录才能看。目前只有「关注」有 —— /feed/listByFollowing 挂的是 JWTAuth */
  auth?: boolean
}

export const FEED_TABS: TabDef[] = [
  { key: 'latest', label: '最新', hint: 'POST /feed/listLatest · 单值游标 latest_time（毫秒）' },
  { key: 'likes', label: '点赞榜', hint: 'POST /feed/listLikesCount · 复合游标 (likes_count, id)' },
  {
    key: 'popularity',
    label: '热门榜',
    hint: 'POST /feed/listByPopularity · 三键游标 (popularity, create_time, id)',
  },
  {
    key: 'following',
    label: '关注',
    hint: 'POST /feed/listByFollowing · 单值游标 latest_time（毫秒）· 必须登录',
    // 同一个 /feed 页，唯一一条**双重鉴权**的接口：
    // feedGroup 整体是 SoftJWTAuth（游客能读），这个空路径子组再叠一层 JWTAuth。
    // 前端的对应物就是"游客连这个 tab 都不给看" —— 不是查完再过滤，是压根不问。
    auth: true,
  },
]

export function isTabKey(v: unknown): v is TabKey {
  return (
    v === 'latest' || v === 'likes' || v === 'popularity' || v === 'following'
  )
}

export function requiresAuth(key: TabKey): boolean {
  return FEED_TABS.find((t) => t.key === key)?.auth === true
}

/** 当前身份下能看见的 tab。游客少一个「关注」 */
export function visibleTabs(loggedIn: boolean): TabDef[] {
  return FEED_TABS.filter((t) => !t.auth || loggedIn)
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
  /**
   * 代数计数，只为了一件事：**丢掉过期的响应**。
   *
   * 场景：关注流的第 2 页正在飞，这时用户点了关注/取关 → invalidate() 把流作废。
   * 不扔掉那次响应的话，它回来会把"关注之前"的那一页写进 items，
   * 而 loaded 又被 ensure() 设成 true —— 于是就固化成了一份**半新半旧**的列表，
   * 除非手动刷新永远好不了。
   *
   * 和 VideoDetailView 里 `if (video.value?.id !== id) return` 是同一个模式，
   * 只是这里的"同一个东西"是游标状态而不是路由参数。
   */
  gen: number
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
    gen: 0,
  }
}

/**
 * 四个 Feed 流的游标状态机。
 *
 * 抽成 composable 的动机：桌面壳和移动壳要的是**同一份数据逻辑**，只有渲染不同。
 * 如果把它写在 FeedView 里，两个端要么复制粘贴一份，要么被迫把视图也写两份。
 *
 * **状态挂在模块作用域，不是每次调用新建。**
 * 因为 App.vue 里没有 `<KeepAlive>`：点卡片进播放页会把 FeedView 卸载，
 * 再回来是重新 setup。状态建在函数里的话，四个流的游标、已加载的每一页、
 * DebugPanel 的跨页重复探针会全部归零 —— 用户按一次返回就得重新从第一页刷。
 * 提上来之后数据活着，配合路由的 scrollBehavior 还能把滚动位置也还原回去。
 *
 * 代价是四个流常驻内存（每流几百条 item 封顶，翻页游标只增不减）。这个交换很划算。
 * 单例由**第一个**调用方决定 limit；目前只有 FeedView 一个调用方
 * （FollowButton 只用下面的 invalidateFeedStream，它刻意不创建单例）。
 */
let singleton: ReturnType<typeof createStreams> | null = null

export function useFeedStream(limit = 12) {
  if (!singleton) singleton = createStreams(limit)
  return singleton
}

/**
 * 作废某个流，**但不创建单例**。
 *
 * FollowButton 关注完要作废「关注流」，可它不该顺手把单例造出来 ——
 * 单例的 limit 由第一个调用方决定，一个按钮来负责这件事说不通
 * （虽然两边默认值现在都是 12，靠巧合对上不值得依赖）。
 * 单例还不存在 = 还没有任何流被加载过 = 没有任何东西需要作废。
 */
export function invalidateFeedStream(key: TabKey) {
  singleton?.invalidate(key)
}

function createStreams(limit: number) {
  const states = reactive<Record<TabKey, StreamState>>({
    latest: newState(),
    likes: newState(),
    popularity: newState(),
    following: newState(),
  })

  const active = ref<TabKey>('latest')
  const cur = computed(() => states[active.value])

  async function load(key: TabKey, reset: boolean) {
    const st = states[key]
    if (st.loading) return
    st.loading = true
    st.error = ''
    const gen = st.gen

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
      } else if (key === 'popularity') {
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
      } else {
        // following：协议和 latest 一模一样，只有数据源不同。
        // 单位也是**毫秒** —— 后端刻意和原项目不一致，为的就是让这两条流
        // 能共用这一个 composable，不出现"这条流要换算、那条不用"的隐式约定
        const res = await listByFollowing({ limit, latest_time: reset ? 0 : st.cTime })
        list = res.video_list
        st.hasMore = res.has_more
        st.cTime = res.next_time
      }

      // 期间被 invalidate 过 → 这份数据描述的是作废之前的世界，丢掉。
      // 注意顺序：判在写 st.* 之前，所以不会污染（游标在上面已经写了，
      // 但那几个字段 invalidate 会重置，且下一次 load 走的是 reset 分支）
      if (st.gen !== gen) return

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
      // 同上：作废之后回来的错误也不该报 —— 用户已经不在看这个流了
      if (st.gen === gen) st.error = e instanceof Error ? e.message : String(e)
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

  /**
   * 把一个流打回"没加载过"，下次 ensure() 会重新拉第一页。
   *
   * 这是**前端版的缓存失效**。已加载的列表活在内存里，底下的数据变了
   * （比如刚关注了一个人）它不会自己知道 —— 不主动作废就一直显示旧列表。
   * 阶段7 在后端做的事情是同一个（invalidateFollowingFeedCache），
   * 只是失效对象换来 Redis 里那份。
   */
  function invalidate(key: TabKey) {
    const st = states[key]
    st.gen += 1 // 让正在飞的那次请求作废（见 StreamState.gen 的说明）
    st.items = []
    st.loaded = false
    st.hasMore = false
    st.dupCount = 0
    st.error = ''
    st.cTime = 0
    st.cLikes = undefined
    st.cID = undefined
    st.cPopularity = undefined
    st.cBefore = undefined
    st.cLatestID = undefined
  }

  return { states, active, cur, load, ensure, setTab, invalidate, limit }
}
