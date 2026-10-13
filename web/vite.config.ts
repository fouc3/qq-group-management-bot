import { defineConfig } from 'vite'
import type { Plugin } from 'vite'
import react from '@vitejs/plugin-react'

/**
 * 把产物从 ES module 改成经典脚本。
 *
 * 原因：`file://` 下打开 dist/index.html 时，Firefox/Chromium 会按 CORS 规则拒绝
 * 执行 `type="module"`（file 源是不透明源），页面就只剩首屏兜底文案。
 * 产物本来就是单入口、不切分的自包含 chunk（见下面的 format: 'iife' +
 * inlineDynamicImports），经典脚本在 file:// 与 HTTP 两种场景下都能跑。
 *
 * 同时去掉 `crossorigin`：Vite 会给注入的 script / link 加 crossorigin，
 * 而这会让浏览器以 CORS 模式去取同目录的 CSS；file:// 下该请求失败，
 * 页面会停在脚本执行前。
 *
 * 关键点：`type="module"` 自带宽限期（defer）语义，改成经典脚本后必须显式补上
 * `defer`，否则脚本会在 <head> 里、`#root` 还没解析出来时就执行。
 * 这两步都是在 generateBundle 阶段改最终 HTML，不影响 HTTP 托管场景。
 */
function classicScriptPlugin(): Plugin {
  return {
    name: 'qgb:classic-script',
    apply: 'build',
    enforce: 'post',
    generateBundle(_options, bundle) {
      for (const file of Object.values(bundle)) {
        if (file.type !== 'asset' || !file.fileName.endsWith('.html')) {
          continue
        }
        if (typeof file.source !== 'string') {
          continue
        }
        file.source = file.source.replace(/ type="module"/g, ' defer').replace(/ crossorigin/g, '')
      }
    },
  }
}

// 产物要交给 Go 侧 `go:embed web/dist` 托管，可能挂在任意子路径下，
// 因此 base 必须是相对路径 './'，所有资源引用都不能带根路径。
export default defineConfig({
  base: './',
  plugins: [react(), classicScriptPlugin()],
  build: {
    outDir: 'dist',
    // dist 里保留了 .gitkeep（保证 go:embed 的目录在干净检出时存在），
    // 构建时绝不能清空输出目录。
    emptyOutDir: false,
    assetsDir: 'assets',
    sourcemap: false,
    // iife 不支持 code splitting：CSS 必须整包进一个文件，
    // 否则样式会被塞进 JS、或者干脆不产出 .css 供 <link> 引用。
    cssCodeSplit: false,
    // 单入口 + 不切分：产物只有 index.html + 一个 js + 一个 css，便于 embed，
    // 也让 iife 包装成立（iife 不支持 code splitting）。
    rollupOptions: {
      output: {
        format: 'iife',
        inlineDynamicImports: true,
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      // 开发期把 /api 转给机器人进程内的 admin 服务，免 CORS。
      '/api': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: true,
      },
    },
  },
})
