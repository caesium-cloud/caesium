import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"

import { StatusGlyph } from "./status-badge";
import { statusMeta } from "@/lib/status";
import { cn } from "@/lib/utils"

const badgeVariants = cva(
  "inline-flex items-center gap-2 text-xs font-normal lowercase text-text-3",
  { variants: { variant: {
    default: "text-text-3", secondary: "text-text-3", destructive: "text-danger", success: "text-success", running: "text-running", cached: "text-cached", outline: "text-text-3",
  } }, defaultVariants: { variant: "default" } }
)

export interface BadgeProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return (
    <div className={cn(badgeVariants({ variant }), className)} {...props}>
      {variant && ["destructive", "success", "running", "cached"].includes(variant) ? <StatusGlyph meta={statusMeta(variant === "destructive" ? "failed" : variant === "success" ? "succeeded" : variant)} /> : null}
      {props.children}
    </div>
  )
}

// eslint-disable-next-line react-refresh/only-export-components
export { Badge, badgeVariants }
