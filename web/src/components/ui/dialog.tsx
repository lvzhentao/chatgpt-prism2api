import * as DialogPrimitive from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import type { HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export const Dialog = DialogPrimitive.Root;
export const DialogTrigger = DialogPrimitive.Trigger;
export const DialogClose = DialogPrimitive.Close;

export function DialogContent({ className, children, ...props }: DialogPrimitive.DialogContentProps) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-ink/40 data-[state=open]:animate-in" />
      <DialogPrimitive.Content
        className={cn(
          "fixed left-1/2 top-1/2 z-50 w-[min(560px,calc(100vw-24px))] -translate-x-1/2 -translate-y-1/2 rounded-lg border border-border bg-bone p-6 shadow-[var(--shadow-flyout)]",
          className,
        )}
        {...props}
      >
        {children}
        <DialogPrimitive.Close className="absolute right-4 top-4 text-ash hover:text-ink">
          <X className="size-4" />
        </DialogPrimitive.Close>
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}

export function DialogTitle({ className, ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <DialogPrimitive.Title className={cn("text-[22px] tracking-[-0.11px] text-ink", className)} {...props} />;
}

export function DialogDesc({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) {
  return <DialogPrimitive.Description className={cn("mt-1 text-[13px] text-ash", className)} {...props} />;
}
