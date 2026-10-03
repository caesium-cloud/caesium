import { cn } from "@/lib/utils"

function Skeleton({
  className,
  ...props
}: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn("rounded-sm bg-obsidian", className)}
      {...props}
    />
  )
}

export { Skeleton }
