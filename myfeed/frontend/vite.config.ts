import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      // 前端(5173)请求 /api/xxx → vite 转发给后端(8080)的 /xxx
      // 同源代理，绕开浏览器跨域(CORS)限制
      // 强制 IPv4：Windows 下 localhost 可能解析成 ::1 导致 ECONNREFUSED
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api/, ''),
      },
    },
  },
})
