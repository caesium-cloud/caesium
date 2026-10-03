import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import './index.css'
import { router } from './router'
import { Toaster } from '@/components/ui/sonner'
import { ThemeProvider } from './components/theme-provider'
import { UTCClockProvider } from '@/components/ui/utc-clock'
import { AuthGate } from './features/auth/AuthGate'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider defaultTheme="dark" storageKey="caesium-ui-theme">
        <UTCClockProvider>
        <AuthGate>
          <RouterProvider router={router} />
        </AuthGate>
        <Toaster />
        </UTCClockProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
)
