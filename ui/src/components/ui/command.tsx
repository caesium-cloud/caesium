"use client"

import * as React from "react"
import { type DialogProps } from "@radix-ui/react-dialog"
import { Command as CommandPrimitive } from "cmdk"
import { Search } from "lucide-react"

import { cn } from "@/lib/utils"
import { Dialog, DialogContent, DialogTitle, DialogDescription } from "@/components/ui/dialog"

const Command = React.forwardRef<
  React.ElementRef<typeof CommandPrimitive>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive>
>(({ className, ...props }, ref) => (
  <CommandPrimitive
    ref={ref}
    className={cn(
      "flex h-full w-full flex-col overflow-hidden rounded-md bg-popover text-popover-foreground",
      className
    )}
    {...props}
  />
))
Command.displayName = CommandPrimitive.displayName

type CommandDialogProps = DialogProps & {
  commandProps?: React.ComponentPropsWithoutRef<typeof CommandPrimitive>;
  contentProps?: React.ComponentPropsWithoutRef<typeof DialogContent>;
}

const CommandDialog = ({ children, commandProps, contentProps, ...props }: CommandDialogProps) => {
  return (
    <Dialog {...props}>
      <DialogContent {...contentProps} className={cn("top-4 flex max-h-[calc(100dvh-2rem)] max-w-2xl translate-y-0 gap-0 overflow-hidden rounded-md p-0 shadow-xl sm:top-[15vh] sm:max-h-[70dvh] [&>button]:right-2 [&>button]:top-2 [&>button]:flex [&>button]:size-8 [&>button]:items-center [&>button]:justify-center", contentProps?.className)}>
        <DialogTitle className="sr-only">Navigation search</DialogTitle>
        <DialogDescription className="sr-only">Find pages, jobs, triggers, or atoms.</DialogDescription>
        <Command
          {...commandProps}
          className={cn(
            "h-auto min-h-0 rounded-none bg-midnight",
            commandProps?.className,
          )}
        >
          {children}
        </Command>
      </DialogContent>
    </Dialog>
  )
}

const CommandInput = React.forwardRef<
  React.ElementRef<typeof CommandPrimitive.Input>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Input>
>(({ className, ...props }, ref) => (
  <div className="flex shrink-0 items-center gap-3 border-b border-border pl-4 pr-12 focus-within:ring-2 focus-within:ring-inset focus-within:ring-ring" cmdk-input-wrapper="">
    <Search aria-hidden="true" className="size-4 shrink-0 text-cyan" />
    <CommandPrimitive.Input
      ref={ref}
      className={cn(
        "flex h-12 min-w-0 w-full bg-transparent py-3 text-sm outline-none focus-visible:outline-none placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-50",
        className
      )}
      {...props}
    />
  </div>
))

CommandInput.displayName = CommandPrimitive.Input.displayName

const CommandList = React.forwardRef<
  React.ElementRef<typeof CommandPrimitive.List>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.List>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.List
    ref={ref}
    className={cn("min-h-0 max-h-[min(25rem,50dvh)] overflow-y-auto overflow-x-hidden overscroll-contain p-2", className)}
    {...props}
  />
))

CommandList.displayName = CommandPrimitive.List.displayName

const CommandEmpty = React.forwardRef<
  React.ElementRef<typeof CommandPrimitive.Empty>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Empty>
>((props, ref) => (
  <CommandPrimitive.Empty
    ref={ref}
    className="py-6 text-center text-sm"
    {...props}
  />
))

CommandEmpty.displayName = CommandPrimitive.Empty.displayName

const CommandGroup = React.forwardRef<
  React.ElementRef<typeof CommandPrimitive.Group>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Group>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Group
    ref={ref}
    className={cn(
      "overflow-hidden text-foreground [&_[cmdk-group-heading]]:px-3 [&_[cmdk-group-heading]]:py-2 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:font-normal [&_[cmdk-group-heading]]:text-muted-foreground",
      className
    )}
    {...props}
  />
))

CommandGroup.displayName = CommandPrimitive.Group.displayName

const CommandSeparator = React.forwardRef<
  React.ElementRef<typeof CommandPrimitive.Separator>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Separator>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Separator
    ref={ref}
    className={cn("my-2 h-px bg-border", className)}
    {...props}
  />
))
CommandSeparator.displayName = CommandPrimitive.Separator.displayName

const CommandItem = React.forwardRef<
  React.ElementRef<typeof CommandPrimitive.Item>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Item>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Item
    ref={ref}
    className={cn(
      "relative flex min-h-10 cursor-pointer gap-4 select-none items-center border-l-2 border-transparent px-3 py-2 text-sm outline-none data-[disabled=true]:pointer-events-none data-[selected=true]:border-cyan data-[selected=true]:bg-cyan/10 data-[selected=true]:text-foreground data-[disabled=true]:opacity-50 [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
      className
    )}
    {...props}
  />
))

CommandItem.displayName = CommandPrimitive.Item.displayName

const CommandShortcut = ({
  className,
  ...props
}: React.HTMLAttributes<HTMLSpanElement>) => {
  return (
    <span
      className={cn(
        "ml-auto text-xs text-muted-foreground",
        className
      )}
      {...props}
    />
  )
}
CommandShortcut.displayName = "CommandShortcut"

export {
  Command,
  CommandDialog,
  CommandInput,
  CommandList,
  CommandEmpty,
  CommandGroup,
  CommandItem,
  CommandShortcut,
  CommandSeparator,
}
