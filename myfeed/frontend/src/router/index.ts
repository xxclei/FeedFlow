import { createRouter, createWebHistory } from 'vue-router'

import { useAuthStore } from '../stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/feed' },
    { path: '/login', component: () => import('../views/LoginView.vue') },
    { path: '/register', component: () => import('../views/RegisterView.vue') },
    {
      // 发现页刻意不加 requiresAuth：后端挂的是 SoftJWTAuth，游客也该能刷流
      path: '/feed',
      component: () => import('../views/FeedView.vue'),
    },
    {
      path: '/tag/:name',
      component: () => import('../views/TagView.vue'),
    },
    {
      path: '/home',
      component: () => import('../views/HomeView.vue'),
      meta: { requiresAuth: true },
    },
    {
      path: '/video',
      component: () => import('../views/VideoView.vue'),
      meta: { requiresAuth: true },
    },
    {
      // 播放页。**刻意不加 requiresAuth**：`/video/getDetail` 是公开接口
      // （router.go 里它挂在没有 JWT 中间件的那一组），游客点卡片也得能进来。
      // 上面那条 `/video` 有 requiresAuth，复制过来时特别容易连 meta 一起抄 —— 抄了就
      // 会把所有未登录用户从发现页一脚踢到登录页。
      //
      // vue-router 把 `/video` 和 `/video/:id` 当两条独立记录（静态段优先级高于参数段），
      // 所以投稿页完全不受影响。
      path: '/video/:id',
      component: () => import('../views/VideoDetailView.vue'),
    },
    // 阶段6+：/following /profile/:id 在这里追加
  ],
  /**
   * 回退时还原滚动位置。
   *
   * 不写这个的话，从详情页返回发现页会落在**顶部附近**：浏览器确实记得你滚到了 3000px，
   * 但返回那一刻文档还没渲染出那么多内容，滚动位置就被夹到了当前文档高度。
   * `savedPosition` 只在 popstate（前进/后退）时才有值，正常跳转是 undefined → 回顶部。
   *
   * 这条要和 useFeedStream 里"状态提到模块作用域"配套：视图重新挂载时列表还在，
   * 文档一上来就有足够高度，还原才有意义。
   */
  scrollBehavior(_to, _from, savedPosition) {
    return savedPosition ?? { top: 0 }
  },
})

// 路由守卫：需要登录的页面，没登录就踢到 /login
router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.meta.requiresAuth && !auth.isLoggedIn) {
    return '/login'
  }
  return true
})

export default router
