/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  resolve: {
    tsconfigPaths: true,
  },
  build: {
    rolldownOptions: {
      output: {
        codeSplitting: {
          minSize: 20_000,
          groups: [
            {
              name: "vendor-react",
              test: /node_modules[\\/](react|react-dom|scheduler|use-sync-external-store)[\\/]/,
              priority: 50,
            },
            {
              name: "vendor-tanstack",
              test: /node_modules[\\/]@tanstack[\\/]/,
              priority: 40,
            },
            {
              name: "vendor-dag",
              test: /node_modules[\\/](reactflow|dagre)[\\/]/,
              priority: 30,
            },
            {
              name: "vendor-charting",
              test: /node_modules[\\/]recharts[\\/]/,
              priority: 30,
            },
            {
              name: "vendor-terminal",
              test: /node_modules[\\/](xterm|xterm-addon-fit)[\\/]/,
              priority: 30,
            },
            {
              name: "vendor-ui",
              test: /node_modules[\\/](@radix-ui|lucide-react|cmdk|sonner|next-themes)[\\/]/,
              priority: 20,
            },
          ],
        },
      },
    },
  },
  server: {
    proxy: {
      '/v1': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      }
    }
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: './vitest.setup.ts',
  }
})
