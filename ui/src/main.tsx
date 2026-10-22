import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient } from '@tanstack/react-query'
import './index.css'
import { router } from './router'
import { Toaster } from '@/components/ui/sonner'
import { ThemeProvider } from './components/theme-provider'
import { UTCClockProvider } from '@/components/ui/utc-clock'
import { AuthGate } from './features/auth/AuthGate'
import { SessionQueryProvider } from './features/auth/SessionQueryProvider'

const createQueryClient = () =>
  new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 15_000,
      },
    },
  })

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider defaultTheme="dark" storageKey="caesium-ui-theme">
      <UTCClockProvider>
        <AuthGate>
          <SessionQueryProvider create={createQueryClient}>
            <RouterProvider router={router} />
          </SessionQueryProvider>
        </AuthGate>
        <Toaster />
      </UTCClockProvider>
    </ThemeProvider>
  </StrictMode>,
)
