import * as SeparatorPrimitive from "@radix-ui/react-separator";
import { cn } from "@/lib/utils";

export function Separator({ className, orientation = "horizontal", ...props }: SeparatorPrimitive.SeparatorProps) {
  return (
    <SeparatorPrimitive.Root
      orientation={orientation}
      className={cn(orientation === "horizontal" ? "h-px w-full bg-stone" : "h-full w-px bg-stone", className)}
      {...props}
    />
  );
}
