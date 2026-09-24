import * as SwitchPrimitive from "@radix-ui/react-switch";
import { cn } from "@/lib/utils";

export function Switch({ className, ...props }: SwitchPrimitive.SwitchProps) {
  return (
    <SwitchPrimitive.Root
      className={cn(
        "peer inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-md border border-border bg-linen transition-colors data-[state=checked]:bg-ink dark:data-[state=checked]:bg-parchment",
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb className="pointer-events-none block size-3.5 translate-x-0.5 rounded-sm bg-bone transition-transform data-[state=checked]:translate-x-[16px] data-[state=checked]:bg-parchment dark:data-[state=checked]:bg-ink" />
    </SwitchPrimitive.Root>
  );
}
