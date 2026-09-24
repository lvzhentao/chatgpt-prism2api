import * as TabsPrimitive from "@radix-ui/react-tabs";
import { cn } from "@/lib/utils";

export const Tabs = TabsPrimitive.Root;
export const TabsList = ({ className, ...props }: TabsPrimitive.TabsListProps) => (
  <TabsPrimitive.List className={cn("flex flex-wrap gap-1 border-b border-border", className)} {...props} />
);
export const TabsTrigger = ({ className, ...props }: TabsPrimitive.TabsTriggerProps) => (
  <TabsPrimitive.Trigger
    className={cn(
      "px-3 py-2 text-[13px] text-ash data-[state=active]:text-ink data-[state=active]:shadow-[inset_0_-1px_0_0_var(--color-ink)]",
      className,
    )}
    {...props}
  />
);
export const TabsContent = ({ className, ...props }: TabsPrimitive.TabsContentProps) => (
  <TabsPrimitive.Content className={cn("pt-4", className)} {...props} />
);
