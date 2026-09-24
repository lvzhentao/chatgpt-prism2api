import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import type { ButtonHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-[14px] font-normal transition-colors duration-150 ease-[cubic-bezier(0.4,0,0.2,1)] disabled:pointer-events-none disabled:opacity-40 cursor-pointer [&_svg]:size-4",
  {
    variants: {
      variant: {
        default: "bg-primary text-primary-foreground hover:opacity-90",
        secondary: "bg-secondary text-secondary-foreground hover:bg-linen dark:hover:bg-stone",
        ghost: "bg-transparent text-foreground/70 hover:underline",
        outline: "border border-border bg-transparent hover:bg-linen/60 dark:hover:bg-linen/20",
        destructive: "bg-crimson text-parchment hover:opacity-90",
        amber: "bg-amber text-parchment hover:opacity-90",
        forest: "bg-forest text-parchment hover:opacity-90",
        ember: "text-ember hover:underline bg-transparent",
      },
      size: {
        default: "h-9 px-3.5 py-2",
        sm: "h-8 px-2.5 text-[13px]",
        lg: "h-10 px-4",
        icon: "size-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

export function Button({
  className,
  variant,
  size,
  asChild,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> &
  VariantProps<typeof buttonVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : "button";
  return <Comp className={cn(buttonVariants({ variant, size, className }))} {...props} />;
}
