"use client"

import { useTheme } from "next-themes"
import { StatusGlyph } from "./status-badge"
import { statusMeta } from "@/lib/status"
import { Toaster as Sonner } from "sonner"

type ToasterProps = React.ComponentProps<typeof Sonner>

const Toaster = ({ ...props }: ToasterProps) => {
  const { theme = "system" } = useTheme()

  return (
    <Sonner
      theme={theme as ToasterProps["theme"]}
      className="toaster group"
      icons={{ success: <StatusGlyph meta={statusMeta("succeeded")} />, error: <StatusGlyph meta={statusMeta("failed")} />, warning: <StatusGlyph meta={statusMeta("paused")} />, info: <StatusGlyph meta={statusMeta("queued")} /> }}
      toastOptions={{
        classNames: {
          toast:
            "group toast group-[.toaster]:bg-midnight group-[.toaster]:text-foreground group-[.toaster]:border-border ",
          description: "group-[.toast]:text-muted-foreground",
          actionButton:
            "group-[.toast]:bg-primary group-[.toast]:text-primary-foreground",
          cancelButton:
            "group-[.toast]:bg-muted group-[.toast]:text-muted-foreground",
        },
      }}
      {...props}
    />
  )
}

export { Toaster }
